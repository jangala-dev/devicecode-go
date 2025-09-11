// services/hal/adaptor_aht20_driver_test.go
package hal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"tinygo.org/x/drivers"
)

// Compile-time check.
var _ drivers.I2C = (*fakeI2C)(nil)

// Scripted AHT20-like fake.
type fakeI2C struct {
	mu         sync.Mutex
	readyAt    time.Time
	calib      bool
	busy       bool
	hraw, traw uint32
}

func newFakeAHT20() *fakeI2C {
	// 25.0°C, 55.0 %RH
	const traw = 393_216 // exact 25.0°C
	const hraw = 576_717 // rounds to 55.0 %RH
	return &fakeI2C{calib: true, hraw: hraw, traw: traw}
}

func (f *fakeI2C) Tx(addr uint16, w, r []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now()

	// Status read
	if len(w) == 1 && w[0] == 0x71 && len(r) == 1 {
		var s byte
		if f.calib {
			s |= 0x08
		}
		if f.busy && now.Before(f.readyAt) {
			s |= 0x80
		}
		r[0] = s
		return nil
	}

	// Trigger
	if len(w) == 3 && w[0] == 0xAC {
		f.busy = true
		f.readyAt = now.Add(30 * time.Millisecond)
		return nil
	}

	// Data read (7 bytes)
	if len(w) == 0 && len(r) == 7 {
		var s byte
		if f.calib {
			s |= 0x08
		}
		if f.busy && now.Before(f.readyAt) {
			s |= 0x80
		} else {
			f.busy = false
		}
		r[0] = s
		h, t := f.hraw, f.traw
		r[1] = byte((h >> 12) & 0xFF)
		r[2] = byte((h >> 4) & 0xFF)
		r[3] = byte(((h & 0xF) << 4) | ((t >> 16) & 0x0F))
		r[4] = byte((t >> 8) & 0xFF)
		r[5] = byte(t & 0xFF)
		r[6] = 0
		return nil
	}

	// Init etc.: accept.
	return nil
}

func TestAHT20Adaptor_TwoPhase(t *testing.T) {
	bus := newFakeAHT20()
	var i2c drivers.I2C = bus

	ad := NewAHT20Adaptor("aht0", i2c, 0x38)

	ctx := context.Background()
	after, err := ad.Trigger(ctx)
	if err != nil {
		t.Fatalf("trigger error: %v", err)
	}

	// Immediately after trigger: should report not ready.
	if _, err := ad.Collect(ctx); err == nil || !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected ErrNotReady immediately after trigger, got: %v", err)
	}

	// Wait for adaptor's suggested interval (driver's TriggerHint) plus margin.
	time.Sleep(after + 10*time.Millisecond)

	sample, err := ad.Collect(ctx)
	if err != nil {
		t.Fatalf("collect error after delay: %v", err)
	}

	// Expect readings for temperature and humidity with fixed-point payloads.
	var gotTemp, gotHum bool
	for _, rd := range sample {
		switch rd.Kind {
		case "temperature":
			m, ok := rd.Payload.(map[string]any)
			if !ok {
				t.Fatalf("temperature payload type: %T", rd.Payload)
			}
			if v, ok := asIntTest(m["deci_c"]); !ok || v != 250 {
				t.Fatalf("temperature deci_c = %v (want 250)", m["deci_c"])
			}
			gotTemp = true

		case "humidity":
			m, ok := rd.Payload.(map[string]any)
			if !ok {
				t.Fatalf("humidity payload type: %T", rd.Payload)
			}
			if v, ok := asIntTest(m["deci_percent"]); !ok || v != 550 {
				t.Fatalf("humidity deci_percent = %v (want 550)", m["deci_percent"])
			}
			gotHum = true
		}
	}
	if !gotTemp || !gotHum {
		t.Fatalf("missing readings: temperature=%v humidity=%v", gotTemp, gotHum)
	}
}

func asIntTest(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case uint:
		return int(x), true
	case uint32:
		return int(x), true
	case uint64:
		return int(x), true
	case float32:
		return int(x), true
	case float64:
		return int(x), true
	default:
		return 0, false
	}
}

// services/hal/hal_integration_test.go
package hal

import (
	"context"
	"testing"
	"time"

	"devicecode-go/bus"

	"tinygo.org/x/drivers"
)

// -----------------------------------------------------------------------------
// Fakes
// -----------------------------------------------------------------------------

// Compile-time check for your existing fake I²C (as in your current test file).
var _ drivers.I2C = (*fakeI2C)(nil)

// fakeI2C + newFakeAHT20 are assumed to exist from your current test harness.

// fakeFactories satisfies both I2CBusFactory and PinFactory.
type fakeFactories struct {
	i2c  drivers.I2C
	pins map[int]GPIOPin
}

func (f fakeFactories) ByID(id string) (drivers.I2C, bool) {
	if id == "i2c0" {
		return f.i2c, true
	}
	return nil, false
}
func (f fakeFactories) ByNumber(n int) (GPIOPin, bool) {
	p, ok := f.pins[n]
	return p, ok
}

// Helpers
func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float32:
		return int(x), true
	case float64:
		return int(x), true
	default:
		return 0, false
	}
}

// -----------------------------------------------------------------------------
// AHT20
// -----------------------------------------------------------------------------

func TestHAL_EndToEnd_AHT20(t *testing.T) {
	b := bus.NewBus(128)
	halConn := b.NewConnection("hal2")
	i2c := newFakeAHT20()

	factory := fakeFactories{
		i2c:  i2c,
		pins: map[int]GPIOPin{}, // not used in this test
	}

	ctx, cancel := context.WithCancel(context.Background())
	go Run(ctx, halConn, factory, factory) // I2C + Pins

	// 1) Wait for HAL 'awaiting_config'
	stateSub := halConn.Subscribe(bus.Topic{"hal", "state"})
	defer halConn.Unsubscribe(stateSub)
	// Cancel *after* all Unsubscribe defers are registered so it runs first at teardown.
	defer cancel()

	waitReady := time.Now().Add(1 * time.Second)
	ready := false
	for time.Now().Before(waitReady) && !ready {
		select {
		case m := <-stateSub.Channel():
			if s, _ := m.Payload.(map[string]any); s != nil &&
				s["level"] == "idle" && s["status"] == "awaiting_config" {
				ready = true
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatal("HAL did not report awaiting_config")
	}

	// 2) Subscribe to capability tree
	valSub := halConn.Subscribe(bus.Topic{"hal", "capability", "#"})
	defer halConn.Unsubscribe(valSub)

	// 3) Publish config
	cfg := map[string]any{
		"version": 1,
		"buses": []map[string]any{
			{"id": "i2c0", "type": "i2c"},
		},
		"devices": []map[string]any{
			{
				"id":      "aht20-0",
				"type":    "aht20",
				"bus_ref": map[string]any{"id": "i2c0", "type": "i2c"},
				"params":  map[string]any{"addr": 56},
			},
		},
	}
	halConn.Publish(halConn.NewMessage(bus.Topic{"config", "hal"}, cfg, false))

	// 4) Discover capability IDs
	var tempID, humID = -1, -1
	waitIDsDeadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(waitIDsDeadline) && (tempID < 0 || humID < 0) {
		select {
		case m := <-valSub.Channel():
			if len(m.Topic) >= 5 && m.Topic[4] == "info" {
				kind, _ := m.Topic[2].(string)
				if id, ok := asInt(m.Topic[3]); ok {
					if kind == "temperature" {
						tempID = id
					} else if kind == "humidity" {
						humID = id
					}
				}
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if tempID < 0 || humID < 0 {
		t.Fatalf("did not receive capability info in time (tempID=%d humID=%d)", tempID, humID)
	}

	// 5) Immediate measurement (request–reply)
	req := halConn.NewMessage(bus.Topic{"hal", "capability", "temperature", tempID, "control", "read_now"}, nil, false)
	rctx, rcancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	_, err := halConn.RequestWait(rctx, req)
	rcancel()
	if err != nil {
		t.Fatalf("read_now request failed: %v", err)
	}

	// 6) Expect a temperature value
	gotValue := false
	valDeadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(valDeadline) && !gotValue {
		select {
		case m := <-valSub.Channel():
			if len(m.Topic) >= 5 && m.Topic[2] == "temperature" && m.Topic[4] == "value" {
				if mm, _ := m.Payload.(map[string]any); mm != nil {
					if dc, ok := toInt(mm["deci_c"]); ok && dc == 250 {
						gotValue = true
					}
				}
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !gotValue {
		t.Fatal("did not receive temperature value after read_now")
	}
}

// -----------------------------------------------------------------------------
// New: End-to-end GPIO coverage
//   - Output control (pwr_en)
//   - Input IRQ events (smbalert)
// -----------------------------------------------------------------------------

func TestHAL_EndToEnd_GPIO(t *testing.T) {
	b := bus.NewBus(128)
	halConn := b.NewConnection("hal_gpio")

	// Pins: output + IRQ-capable input.
	pwr := &fakePin{num: 2}
	alert := &fakeIRQPin{fakePin: fakePin{num: 3, level: true}}

	factory := fakeFactories{
		i2c:  newFakeAHT20(),                    // unused here
		pins: map[int]GPIOPin{2: pwr, 3: alert}, // available to HAL
	}

	ctx, cancel := context.WithCancel(context.Background())
	go Run(ctx, halConn, factory, factory)

	// Subscriptions used across the test
	stateSub := halConn.Subscribe(bus.Topic{"hal", "state"})
	dbgSub := halConn.Subscribe(bus.Topic{"hal", "debug"})
	capSub := halConn.Subscribe(bus.Topic{"hal", "capability", "#"})
	defer halConn.Unsubscribe(stateSub)
	defer halConn.Unsubscribe(dbgSub)
	defer halConn.Unsubscribe(capSub)
	// Cancel first during teardown, then unsubscribe (LIFO), to avoid publishing into closed chans.
	defer cancel()

	// Await initial idle/awaiting_config
	deadline := time.Now().Add(1 * time.Second)
	for ok := false; time.Now().Before(deadline) && !ok; {
		select {
		case m := <-stateSub.Channel():
			if s, _ := m.Payload.(map[string]any); s != nil &&
				s["level"] == "idle" && s["status"] == "awaiting_config" {
				ok = true
			}
		case <-time.After(10 * time.Millisecond):
		}
		if ok {
			break
		}
	}

	// Send config with two GPIOs
	cfg := map[string]any{
		"version": 1,
		"devices": []map[string]any{
			{"id": "pwr_en", "type": "gpio", "params": map[string]any{"pin": 2, "mode": "output", "initial": true}},
			{"id": "smbalert", "type": "gpio", "params": map[string]any{
				"pin": 3, "mode": "input", "pull": "up", "irq": map[string]any{"edge": "falling", "debounce_ms": 2}}},
		},
	}
	halConn.Publish(halConn.NewMessage(bus.Topic{"config", "hal"}, cfg, false))

	// Wait for ready/configured (or an error), capturing any debug output along the way
	ready := false
	deadline = time.Now().Add(1 * time.Second)
	var debugBuf []string
	for time.Now().Before(deadline) && !ready {
		select {
		case m := <-stateSub.Channel():
			if s, _ := m.Payload.(map[string]any); s != nil {
				if s["level"] == "ready" && s["status"] == "configured" {
					ready = true
				}
				if s["level"] == "error" {
					t.Fatalf("HAL error state: %v", s)
				}
			}
		case m := <-dbgSub.Channel():
			if s, ok := m.Payload.(string); ok {
				debugBuf = append(debugBuf, s)
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatalf("HAL did not reach ready/configured; debug=%v", debugBuf)
	}

	// Discover GPIO capability IDs via retained info
	var outID, inID = -1, -1
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && (outID < 0 || inID < 0) {
		select {
		case m := <-capSub.Channel():
			if len(m.Topic) >= 5 && m.Topic[2] == "gpio" && m.Topic[4] == "info" {
				id, ok := asInt(m.Topic[3])
				if !ok {
					continue
				}
				info, _ := m.Payload.(map[string]any)
				if info == nil {
					continue
				}
				switch info["mode"] {
				case "output":
					outID = id
				case "input":
					inID = id
				}
			}
		case m := <-dbgSub.Channel():
			if s, ok := m.Payload.(string); ok {
				debugBuf = append(debugBuf, s)
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if outID < 0 || inID < 0 {
		t.Fatalf("failed to learn GPIO capability IDs (outID=%d inID=%d); debug=%v", outID, inID, debugBuf)
	}

	// Output control
	reqSetLow := halConn.NewMessage(bus.Topic{"hal", "capability", "gpio", outID, "control", "set"},
		map[string]any{"level": 0}, false)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	if _, err := halConn.RequestWait(ctx1, reqSetLow); err != nil {
		t.Fatalf("set low failed: %v; debug=%v", err, debugBuf)
	}
	cancel1()
	if pwr.level != false {
		t.Fatalf("pwr_en physical level expected false, got %v", pwr.level)
	}

	// Input IRQ: event + state
	evSub := halConn.Subscribe(bus.Topic{"hal", "capability", "gpio", inID, "event"})
	stSub := halConn.Subscribe(bus.Topic{"hal", "capability", "gpio", inID, "state"})
	defer halConn.Unsubscribe(evSub)
	defer halConn.Unsubscribe(stSub)

	alert.trigger(false) // falling edge

	gotEvent, gotState := false, false
	deadline = time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) && (!gotEvent || !gotState) {
		select {
		case m := <-evSub.Channel():
			if mm, _ := m.Payload.(map[string]any); mm != nil {
				if mm["edge"] == "falling" {
					if lvl, ok := toInt(mm["level"]); ok && lvl == 0 {
						gotEvent = true
					}
				}
			}
		case m := <-stSub.Channel():
			if mm, _ := m.Payload.(map[string]any); mm != nil {
				if lvl, ok := toInt(mm["level"]); ok && lvl == 0 {
					gotState = true
				}
			}
		case m := <-dbgSub.Channel():
			if s, ok := m.Payload.(string); ok {
				debugBuf = append(debugBuf, s)
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !gotEvent || !gotState {
		t.Fatalf("missing gpio event/state (event=%v state=%v); debug=%v", gotEvent, gotState, debugBuf)
	}
}

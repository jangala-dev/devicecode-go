// services/hal/internal/devices/aht20/driver_tinygo.go
//go:build rp2040 || rp2350

package aht20

import (
	"fmt"
	"time"

	"devicecode-go/drivers/aht20"
	"devicecode-go/services/hal/internal/halcore"
)

var errNotReady = fmt.Errorf("not ready")

// aht20Device is an internal interface to decouple adaptor from the driver type.
type aht20Device interface {
	Configure(addr uint16, pollInterval time.Duration, collectTimeout time.Duration, triggerHint time.Duration)
	Trigger() error
	TriggerHint() time.Duration
	Collect(out *aht20Sample) error
}

type aht20Sample struct {
	deciC  int32
	deciRH int32
}

func (s *aht20Sample) DeciCelsius() int32     { return s.deciC }
func (s *aht20Sample) DeciRelHumidity() int32 { return s.deciRH }

type devWrap struct {
	dev aht20.Device
}

func newAHT20(bus halcore.I2C, addr uint16) aht20Device {
	// The TinyGo driver expects tinygo.org/x/drivers.I2C; our halcore.I2C is compatible.
	d := aht20.New(bus)
	return &devWrap{dev: d}
}

func (w *devWrap) Configure(addr uint16, pollInterval, collectTimeout, triggerHint time.Duration) {
	w.dev.Configure(aht20.Config{
		Address:        addr,
		PollInterval:   pollInterval,
		CollectTimeout: collectTimeout,
		TriggerHint:    triggerHint,
	})
}

func (w *devWrap) Trigger() error             { return w.dev.Trigger() }
func (w *devWrap) TriggerHint() time.Duration { return w.dev.TriggerHint() }

func (w *devWrap) Collect(out *aht20Sample) error {
	var s aht20.Sample
	if err := w.dev.Collect(&s); err != nil {
		if err == aht20.ErrNotReady {
			return errNotReady
		}
		return err
	}
	out.deciC = s.DeciCelsius()
	out.deciRH = s.DeciRelHumidity()
	return nil
}

func (a *adaptor) configure(addr uint16) {
	a.dev.Configure(addr, 15*time.Millisecond, 250*time.Millisecond, 80*time.Millisecond)
}

// services/hal/internal/devices/aht20/driver_std.go
//go:build linux && arm64 && !(rp2040 || rp2350)

package aht20

import (
	"errors"
	"time"

	"devicecode-go/services/hal/internal/halcore"
)

var (
	errNotReady = errors.New("not ready")
	errNoStd    = errors.New("aht20 not supported on standard Go build; inject fakes in tests")
)

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

type devWrap struct{}

func newAHT20(_ halcore.I2C, _ uint16) aht20Device { return &devWrap{} }

func (w *devWrap) Configure(uint16, time.Duration, time.Duration, time.Duration) {}
func (w *devWrap) Trigger() error                                                { return errNoStd }
func (w *devWrap) TriggerHint() time.Duration                                    { return 0 }
func (w *devWrap) Collect(*aht20Sample) error                                    { return errNoStd }

func (a *adaptor) configure(uint16) {}

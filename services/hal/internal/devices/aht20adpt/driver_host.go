// services/hal/internal/devices/aht20/driver_host.go
//go:build !rp2040 && !rp2350

package aht20adpt

import (
	"errors"
	"time"

	"devicecode-go/services/hal/internal/halcore"
)

// -----------------------------------------------------------------------------
// Shared types expected by adaptor.go (host-side definitions)
// -----------------------------------------------------------------------------

var errNotReady = errors.New("not_ready")

// aht20Device is the internal interface used by adaptor.go.
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

// adaptor.configure used by adaptor.go; mirrors the MCU defaults.
func (a *adaptor) configure(addr uint16) {
	a.dev.Configure(addr, 15*time.Millisecond, 250*time.Millisecond, 80*time.Millisecond)
}

// -----------------------------------------------------------------------------
// Host simulator implementing aht20Device
// -----------------------------------------------------------------------------

type simAHT struct {
	addr        uint16
	pollInt     time.Duration
	collectTO   time.Duration
	triggerHint time.Duration
	readyAt     time.Time

	sampleIndex   int
	lastTempDeciC int32
	lastHumDeci   int32
}

// newAHT20 signature must match what adaptor.go expects.
func newAHT20(_ halcore.I2C, addr uint16) aht20Device {
	return &simAHT{addr: addr}
}

func (s *simAHT) Configure(addr uint16, pollInterval, collectTimeout, triggerHint time.Duration) {
	s.addr = addr
	if pollInterval <= 0 {
		pollInterval = 1 * time.Millisecond
	}
	if collectTimeout <= 0 {
		collectTimeout = 20 * time.Millisecond
	}
	if triggerHint <= 0 {
		triggerHint = 10 * time.Millisecond
	}
	s.pollInt = pollInterval
	s.collectTO = collectTimeout
	s.triggerHint = triggerHint
}

func (s *simAHT) Trigger() error {
	s.readyAt = time.Now().Add(s.triggerHint)
	return nil
}

func (s *simAHT) TriggerHint() time.Duration { return s.triggerHint }

func (s *simAHT) Collect(out *aht20Sample) error {
	if time.Now().Before(s.readyAt) {
		return errNotReady
	}
	// Deterministic monotonic readings for tests.
	s.sampleIndex++
	s.lastTempDeciC = 230 + int32(s.sampleIndex) // e.g. 23.1°C, 23.2°C, …
	s.lastHumDeci = 500 + int32(s.sampleIndex*2) // e.g. 50.2%RH, 50.4%RH, …
	out.deciC = s.lastTempDeciC
	out.deciRH = s.lastHumDeci
	return nil
}

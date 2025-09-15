// services/hal/internal/platform/factories_rp2xx.go
//go:build rp2040 || rp2350

package platform

import (
	"machine"

	"tinygo.org/x/drivers"

	halcore "devicecode-go/services/hal/internal/halcore"
)

// -----------------------------------------------------------------------------
// Defaults used by hal.Run on Raspberry Pi Pico / Pico 2 (RP2 family)
// -----------------------------------------------------------------------------

// DefaultI2CFactory configures i2c0 and i2c1 with board-default pins at 400 kHz.
func DefaultI2CFactory() halcore.I2CBusFactory {
	f := &rp2I2CFactory{buses: make(map[string]drivers.I2C)}

	// i2c0 @ 400 kHz on default pins.
	b0 := machine.I2C0
	_ = b0.Configure(machine.I2CConfig{
		Frequency: 400 * machine.KHz,
		SDA:       machine.I2C0_SDA_PIN,
		SCL:       machine.I2C0_SCL_PIN,
	})
	f.buses["i2c0"] = b0

	b1 := machine.I2C1
	_ = b1.Configure(machine.I2CConfig{
		Frequency: 400 * machine.KHz,
		SDA:       machine.I2C1_SDA_PIN,
		SCL:       machine.I2C1_SCL_PIN,
	})
	f.buses["i2c1"] = b1

	return f
}

// DefaultPinFactory returns a GPIO factory that maps logical numbers directly
// to machine.Pin(n). This matches Pico/Pico 2 GP numbering.
func DefaultPinFactory() halcore.PinFactory { return rp2PinFactory{} }

// ---- I²C implementation ----

type rp2I2CFactory struct {
	buses map[string]drivers.I2C
}

func (f *rp2I2CFactory) ByID(id string) (drivers.I2C, bool) {
	b, ok := f.buses[id]
	return b, ok
}

// ---- GPIO implementation (includes IRQ support) ----

type rp2PinFactory struct{}

func (rp2PinFactory) ByNumber(n int) (halcore.GPIOPin, bool) {
	// Constrain to RP2’s user GPIOs (GP0..GP28).
	if n < 0 || n > 28 {
		return nil, false
	}
	return &rp2Pin{p: machine.Pin(n), n: n}, true
}

type rp2Pin struct {
	p machine.Pin
	n int
}

func (r *rp2Pin) ConfigureInput(pull halcore.Pull) error {
	var mode machine.PinMode
	switch pull {
	case halcore.PullUp:
		mode = machine.PinInputPullup
	case halcore.PullDown:
		mode = machine.PinInputPulldown
	default:
		mode = machine.PinInput
	}
	r.p.Configure(machine.PinConfig{Mode: mode})
	return nil
}

func (r *rp2Pin) ConfigureOutput(initial bool) error {
	r.p.Configure(machine.PinConfig{Mode: machine.PinOutput})
	r.p.Set(initial)
	return nil
}

func (r *rp2Pin) Set(level bool) { r.p.Set(level) }
func (r *rp2Pin) Get() bool      { return r.p.Get() }

func (r *rp2Pin) Toggle() {
	if r.p.Get() {
		r.p.Low()
	} else {
		r.p.High()
	}
}

func (r *rp2Pin) Number() int { return r.n }

// IRQ support. The RP2 port provides SetInterrupt with PinChange flags.
func (r *rp2Pin) SetIRQ(edge halcore.Edge, handler func()) error {
	change := toPinChange(edge)
	return r.p.SetInterrupt(change, func(machine.Pin) { handler() })
}

func (r *rp2Pin) ClearIRQ() error {
	var zero machine.PinChange
	return r.p.SetInterrupt(zero, nil)
}

func toPinChange(e halcore.Edge) machine.PinChange {
	switch e {
	case halcore.EdgeRising:
		return machine.PinRising
	case halcore.EdgeFalling:
		return machine.PinFalling
	case halcore.EdgeBoth:
		return machine.PinToggle
	default:
		// Zero value is a no-op/disabled.
		var zero machine.PinChange
		return zero
	}
}

//go:build rp2040

package provider

import (
	"machine"

	"devicecode-go/services/hal/internal/core"
)

type rp2PinFactory struct{}
type rp2Pin struct {
	p machine.Pin
	n int
}

func (rp2PinFactory) ByNumber(n int) (core.GPIOPin, bool) {
	if n < 0 || n > 28 {
		return nil, false
	}
	return &rp2Pin{p: machine.Pin(n), n: n}, true
}

func (r *rp2Pin) ConfigureInput(p core.Pull) error {
	var mode machine.PinMode
	switch p {
	case core.PullUp:
		mode = machine.PinInputPullup
	case core.PullDown:
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
func (r *rp2Pin) Set(b bool) { r.p.Set(b) }
func (r *rp2Pin) Get() bool  { return r.p.Get() }
func (r *rp2Pin) Toggle() {
	if r.p.Get() {
		r.p.Low()
	} else {
		r.p.High()
	}
}
func (r *rp2Pin) Number() int { return r.n }

func NewPinFactory() core.PinFactory { return rp2PinFactory{} }

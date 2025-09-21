//go:build rp2040

package provider

import (
	"sync"

	"devicecode-go/services/hal/internal/core"
	"machine"
)

// Concrete GPIO handle
type rp2Pin struct {
	p machine.Pin
	n int
}

func (r *rp2Pin) Number() int { return r.n }
func (r *rp2Pin) ConfigureInput(pull core.Pull) error {
	var mode machine.PinMode
	switch pull {
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

// Unified resource registry (GPIO today; bus methods stubbed)
type gpioRegistry struct {
	mu    sync.Mutex
	used  map[int]string  // pin -> devID
	cache map[int]*rp2Pin // pin -> handle
}

func NewResourceRegistry() core.ResourceRegistry {
	return &gpioRegistry{
		used:  make(map[int]string),
		cache: make(map[int]*rp2Pin),
	}
}

// ---- core.ResourceRegistry implementation ----

func (g *gpioRegistry) ClassOf(id core.ResourceID) (core.BusClass, bool) { return 0, false }
func (g *gpioRegistry) Txn(id core.ResourceID) (core.TxnOwner, bool)     { return nil, false }
func (g *gpioRegistry) Stream(id core.ResourceID) (core.StreamOwner, bool) {
	return nil, false
}

func (g *gpioRegistry) lookup(n int) (*rp2Pin, bool) {
	if n < 0 || n > 28 {
		return nil, false
	}
	if p, ok := g.cache[n]; ok {
		return p, true
	}
	h := &rp2Pin{p: machine.Pin(n), n: n}
	g.cache[n] = h
	return h, true
}

func (g *gpioRegistry) ClaimGPIO(devID string, n int) (core.GPIOHandle, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.lookup(n); !ok {
		return nil, core.ErrUnknownPin
	}
	if owner, inUse := g.used[n]; inUse && owner != "" {
		return nil, core.ErrPinInUse
	}
	g.used[n] = devID
	return g.cache[n], nil
}

func (g *gpioRegistry) ReleaseGPIO(devID string, n int) {
	g.mu.Lock()
	if owner, ok := g.used[n]; ok && owner == devID {
		delete(g.used, n)
	}
	g.mu.Unlock()
}

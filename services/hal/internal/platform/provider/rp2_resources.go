//go:build rp2040

package provider

import (
	"sync"
	"time"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/platform/boards"
	"devicecode-go/types"
	"machine"
)

// Ensure the provider satisfies the registry contract at compile time.
var _ core.ResourceRegistry = (*gpioRegistry)(nil)

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

	evCh chan core.Event // owner→HAL events
}

func NewResourceRegistry() core.ResourceRegistry {
	return &gpioRegistry{
		used:  make(map[int]string),
		cache: make(map[int]*rp2Pin),
		evCh:  make(chan core.Event, 64),
	}
}

// ---- core.ResourceRegistry implementation ----

func (g *gpioRegistry) ClassOf(id core.ResourceID) (core.BusClass, bool) {
	// No buses exposed yet on this provider.
	return 0, false
}

// Transactional buses (I²C) — stubs for now
func (g *gpioRegistry) ClaimI2C(devID string, id core.ResourceID) (core.I2COwner, error) {
	return nil, core.ErrUnknownBus
}
func (g *gpioRegistry) ReleaseI2C(devID string, id core.ResourceID) {}

// Stream buses — stubs for now
func (g *gpioRegistry) ClaimStream(devID string, id core.ResourceID) (core.StreamOwner, error) {
	return nil, core.ErrUnknownBus
}
func (g *gpioRegistry) ReleaseStream(devID string, id core.ResourceID) {}

func (g *gpioRegistry) Events() <-chan core.Event { return g.evCh }

// GPIO lookup/claim

func (g *gpioRegistry) lookup(n int) (*rp2Pin, bool) {
	min, max := boards.SelectedBoard.GPIOMin, boards.SelectedBoard.GPIOMax
	if n < min || n > max {
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

// ---- GPIO owner operations (synchronous but emit events to HAL) ----

func (g *gpioRegistry) publish(devID string, kind types.Kind, payload any) {
	select {
	case g.evCh <- core.Event{
		DevID:   devID,
		Kind:    kind,
		Payload: payload,
		TSms:    time.Now().UnixMilli(),
	}:
	default:
		// Drop under pressure to protect system; state remains last good.
	}
}

func (g *gpioRegistry) publishErr(devID string, kind types.Kind, code string) {
	select {
	case g.evCh <- core.Event{
		DevID: devID,
		Kind:  kind,
		Err:   code,
		TSms:  time.Now().UnixMilli(),
	}:
	default:
	}
}

// Expose small, non-blocking ops for devices to call (no extra goroutines).

func (g *gpioRegistry) GPIOSet(devID string, pin int, level bool) (core.EnqueueResult, error) {
	g.mu.Lock()
	h, ok := g.cache[pin]
	g.mu.Unlock()
	if !ok {
		return core.EnqueueResult{OK: false, Error: "unknown_pin"}, nil
	}
	h.Set(level)
	var v uint8
	if level {
		v = 1
	}
	g.publish(devID, types.KindLED, types.LEDValue{Level: v})
	return core.EnqueueResult{OK: true}, nil
}

func (g *gpioRegistry) GPIOToggle(devID string, pin int) (core.EnqueueResult, error) {
	g.mu.Lock()
	h, ok := g.cache[pin]
	g.mu.Unlock()
	if !ok {
		return core.EnqueueResult{OK: false, Error: "unknown_pin"}, nil
	}
	h.Toggle()
	var v uint8
	if h.Get() {
		v = 1
	}
	g.publish(devID, types.KindLED, types.LEDValue{Level: v})
	return core.EnqueueResult{OK: true}, nil
}

func (g *gpioRegistry) GPIORead(devID string, pin int) (core.EnqueueResult, error) {
	g.mu.Lock()
	h, ok := g.cache[pin]
	g.mu.Unlock()
	if !ok {
		return core.EnqueueResult{OK: false, Error: "unknown_pin"}, nil
	}
	var v uint8
	if h.Get() {
		v = 1
	}
	g.publish(devID, types.KindLED, types.LEDValue{Level: v})
	return core.EnqueueResult{OK: true}, nil
}

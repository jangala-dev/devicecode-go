// services/hal/internal/platform/factories_host.go
//go:build !rp2040 && !rp2350

package platform

import (
	"sync"
	"time"

	"devicecode-go/services/hal/internal/halcore"

	"tinygo.org/x/drivers"
)

// ----------------------------- I²C (host) ------------------------------------

// HostI2C implements tinygo drivers.I2C for host-side tests.
type HostI2C struct {
	mu     sync.Mutex
	LastTx struct {
		Addr uint16
		W    []byte
		Rn   int
	}
}

func (h *HostI2C) Tx(addr uint16, w, r []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.LastTx.Addr = addr
	h.LastTx.W = append([]byte(nil), w...)
	h.LastTx.Rn = len(r)
	// No emulation necessary for current tests.
	return nil
}

type hostI2CFactory struct {
	buses map[string]drivers.I2C
}

func (f *hostI2CFactory) ByID(id string) (drivers.I2C, bool) {
	b, ok := f.buses[id]
	return b, ok
}

// DefaultI2CFactory creates inert host I²C buses "i2c0" and "i2c1".
func DefaultI2CFactory() halcore.I2CBusFactory {
	return &hostI2CFactory{
		buses: map[string]drivers.I2C{
			"i2c0": &HostI2C{},
			"i2c1": &HostI2C{},
		},
	}
}

// ----------------------------- GPIO (host) -----------------------------------

// FakePin implements GPIOPin and IRQPin for host-side tests.
type FakePin struct {
	mu       sync.RWMutex
	number   int
	level    bool
	modeOut  bool
	irqEdge  halcore.Edge
	irqFunc  func()
	debounce time.Duration
	lastIRQ  time.Time
}

func (p *FakePin) ConfigureInput(_ halcore.Pull) error {
	p.mu.Lock()
	p.modeOut = false
	p.mu.Unlock()
	return nil
}

func (p *FakePin) ConfigureOutput(initial bool) error {
	p.mu.Lock()
	p.modeOut = true
	p.level = initial
	p.mu.Unlock()
	return nil
}

func (p *FakePin) Set(level bool) {
	p.mu.Lock()
	old := p.level
	p.level = level
	edge := edgeFrom(old, level)
	irq := p.irqFunc
	want := irqWanted(p.irqEdge, edge)
	deb := p.debounce
	last := p.lastIRQ
	now := time.Now()
	if want && (deb == 0 || now.Sub(last) >= deb) {
		p.lastIRQ = now
		p.mu.Unlock()
		if irq != nil {
			irq() // ISR-style callback used by gpioirq.Worker
		}
		return
	}
	p.mu.Unlock()
}

func (p *FakePin) Get() bool {
	p.mu.RLock()
	v := p.level
	p.mu.RUnlock()
	return v
}

func (p *FakePin) Toggle() { p.Set(!p.Get()) }

func (p *FakePin) Number() int { return p.number }

func (p *FakePin) SetIRQ(edge halcore.Edge, handler func()) error {
	p.mu.Lock()
	p.irqEdge = edge
	p.irqFunc = handler
	p.mu.Unlock()
	return nil
}

func (p *FakePin) ClearIRQ() error {
	p.mu.Lock()
	p.irqEdge = halcore.EdgeNone
	p.irqFunc = nil
	p.mu.Unlock()
	return nil
}

func edgeFrom(old, new bool) halcore.Edge {
	switch {
	case !old && new:
		return halcore.EdgeRising
	case old && !new:
		return halcore.EdgeFalling
	default:
		return halcore.EdgeNone
	}
}

func irqWanted(cfg, seen halcore.Edge) bool {
	switch cfg {
	case halcore.EdgeBoth:
		return seen == halcore.EdgeRising || seen == halcore.EdgeFalling
	default:
		return cfg == seen
	}
}

// HostPinFactory returns stable *FakePin instances per number.
type HostPinFactory struct {
	mu   sync.Mutex
	pins map[int]*FakePin
}

func (f *HostPinFactory) ByNumber(n int) (halcore.GPIOPin, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pins == nil {
		f.pins = make(map[int]*FakePin)
	}
	p, ok := f.pins[n]
	if !ok {
		p = &FakePin{number: n}
		f.pins[n] = p
	}
	return p, true
}

// Get exposes the underlying *FakePin for tests (e.g. to drive IRQ edges).
func (f *HostPinFactory) Get(n int) (*FakePin, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pins[n]
	return p, ok
}

// DefaultPinFactory provides a host GPIO factory.
func DefaultPinFactory() halcore.PinFactory {
	return &HostPinFactory{pins: make(map[int]*FakePin)}
}

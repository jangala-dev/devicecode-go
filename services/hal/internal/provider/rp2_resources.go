//go:build rp2040

package provider

import (
	"sync"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/provider/boards"
	"devicecode-go/services/hal/internal/provider/setups"
	"machine"
)

// Ensure the provider satisfies the contracts at compile time.
var _ core.ResourceRegistry = (*rp2Registry)(nil)

// ---- Concrete GPIO handle ----

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

// ---- I²C owner (one worker per bus; serialises hardware access) ----

type i2cOwner struct {
	id   core.ResourceID
	hw   *machine.I2C
	jobs chan func(bus core.I2CBus) error
	quit chan struct{}
}

func newI2COwner(id core.ResourceID, hw *machine.I2C) *i2cOwner {
	o := &i2cOwner{
		id:   id,
		hw:   hw,
		jobs: make(chan func(core.I2CBus) error, 16),
		quit: make(chan struct{}),
	}
	go o.loop()
	return o
}

// thin adapter to satisfy core.I2CBus inside the worker
type txBus struct{ hw *machine.I2C }

func (b txBus) Tx(addr uint16, w, r []byte) error { return b.hw.Tx(addr, w, r) }

func (o *i2cOwner) loop() {
	b := txBus{hw: o.hw}
	for {
		select {
		case job := <-o.jobs:
			_ = job(b) // swallow error or add telemetry if desired
		case <-o.quit:
			return
		}
	}
}

// core.I2COwner
func (o *i2cOwner) Tx(addr uint16, w []byte, r []byte, _ int) error {
	// Direct synchronous transaction (caller blocks).
	return o.hw.Tx(addr, w, r)
}
func (o *i2cOwner) TryEnqueue(job func(bus core.I2CBus) error) bool {
	select {
	case o.jobs <- job:
		return true
	default:
		return false
	}
}

// ---- Unified resource registry (GPIO + I2C owners) ----

type rp2Registry struct {
	mu sync.Mutex

	// GPIO
	usedGPIO map[int]string  // pin -> devID
	gpio     map[int]*rp2Pin // pin -> handle

	// I²C
	i2cOwners map[core.ResourceID]*i2cOwner
}

// Accept the selected plan here to break the provider<->platform cycle.
func NewResourceRegistry(plan setups.ResourcePlan) *rp2Registry {
	r := &rp2Registry{
		usedGPIO:  make(map[int]string),
		gpio:      make(map[int]*rp2Pin),
		i2cOwners: make(map[core.ResourceID]*i2cOwner),
	}

	// Instantiate I²C owners from the provided plan (pins and frequency).
	for _, p := range plan.I2C {
		var hw *machine.I2C
		switch p.ID {
		case "i2c0":
			hw = machine.I2C0
		case "i2c1":
			hw = machine.I2C1
		default:
			continue
		}
		// Configure pins & bus frequency.
		sda := machine.Pin(p.SDA)
		scl := machine.Pin(p.SCL)
		sda.Configure(machine.PinConfig{Mode: machine.PinI2C})
		scl.Configure(machine.PinConfig{Mode: machine.PinI2C})
		hw.Configure(machine.I2CConfig{
			SCL:       scl,
			SDA:       sda,
			Frequency: p.Hz,
		})
		r.i2cOwners[core.ResourceID(p.ID)] = newI2COwner(core.ResourceID(p.ID), hw)
	}

	return r
}

// ---- core.ResourceRegistry implementation ----

func (r *rp2Registry) ClassOf(id core.ResourceID) (core.BusClass, bool) {
	switch string(id) {
	case "i2c0", "i2c1":
		return core.BusTransactional, true
	}
	// No other buses exposed yet on this provider.
	return 0, false
}

// Transactional buses (I²C)
func (r *rp2Registry) ClaimI2C(devID string, id core.ResourceID) (core.I2COwner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.i2cOwners[id]
	if o == nil {
		return nil, core.ErrUnknownBus
	}
	return o, nil
}
func (r *rp2Registry) ReleaseI2C(devID string, id core.ResourceID) {
	// Nothing to do for now. Owners are long-lived per bus.
}

// Stream buses — still stubs
func (r *rp2Registry) ClaimStream(devID string, id core.ResourceID) (core.StreamOwner, error) {
	return nil, core.ErrUnknownBus
}
func (r *rp2Registry) ReleaseStream(devID string, id core.ResourceID) {}

// ---- GPIO lookup/claim ----

func (r *rp2Registry) lookupGPIO(n int) (*rp2Pin, bool) {
	min, max := boards.SelectedBoard.GPIOMin, boards.SelectedBoard.GPIOMax
	if n < min || n > max {
		return nil, false
	}
	if p, ok := r.gpio[n]; ok {
		return p, true
	}
	h := &rp2Pin{p: machine.Pin(n), n: n}
	r.gpio[n] = h
	return h, true
}

func (r *rp2Registry) ClaimGPIO(devID string, n int) (core.GPIOHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.lookupGPIO(n); !ok {
		return nil, core.ErrUnknownPin
	}
	if owner, inUse := r.usedGPIO[n]; inUse && owner != "" {
		return nil, core.ErrPinInUse
	}
	r.usedGPIO[n] = devID
	return r.gpio[n], nil
}

func (r *rp2Registry) ReleaseGPIO(devID string, n int) {
	r.mu.Lock()
	if owner, ok := r.usedGPIO[n]; ok && owner == devID {
		delete(r.usedGPIO, n)
	}
	r.mu.Unlock()
}

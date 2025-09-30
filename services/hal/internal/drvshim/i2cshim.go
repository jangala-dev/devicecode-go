package drvshim

import "devicecode-go/services/hal/internal/core"

// I2C adapts either a core.I2COwner (direct Tx) or a raw core.I2CBus (worker job context)
// to the tinygo driver Tx shape.
type I2C struct {
	o         core.I2COwner // optional
	raw       core.I2CBus   // optional
	timeoutMS int
}

func NewI2C(owner core.I2COwner) I2C {
	return I2C{o: owner, timeoutMS: 25}
}

// NewI2CFromBus constructs a shim bound to a raw per-job bus.
// Use this inside TryEnqueue jobs.
func NewI2CFromBus(bus core.I2CBus) I2C {
	return I2C{raw: bus, timeoutMS: 25}
}

func (s I2C) WithTimeout(ms int) I2C {
	if ms > 0 {
		s.timeoutMS = ms
	}
	return s
}

// Tx delegates to the available backend. When 'raw' is set we are executing
// inside the bus worker; otherwise we call into the owner with an optional timeout.
func (s I2C) Tx(addr uint16, w, r []byte) error {
	if s.raw != nil {
		return s.raw.Tx(addr, w, r)
	}
	return s.o.Tx(addr, w, r, s.timeoutMS)
}

// HotI2C implements tinygo's drivers.I2C over a rebindable core.I2CBus.
// It is not concurrency-safe; call Bind() and then use it from the same goroutine.
type HotI2C struct {
	b core.I2CBus
}

// Bind swaps the underlying worker-context bus used by Tx.
func (h *HotI2C) Bind(bus core.I2CBus) { h.b = bus }

// Tx forwards to the currently bound worker bus.
// It must only be called after a successful Bind in the same job.
func (h *HotI2C) Tx(addr uint16, w, r []byte) error {
	return h.b.Tx(addr, w, r)
}

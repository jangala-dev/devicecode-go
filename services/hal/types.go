// services/hal/types.go
package hal

import (
	"context"
	"time"

	"tinygo.org/x/drivers"
)

// Reading is one datum for one capability kind.
type Reading struct {
	Kind    string // e.g. "temperature", "humidity", "voltage"
	Payload any    // JSON-serialisable payload (fixed-point, struct, etc.)
	TsMs    int64  // producer timestamp
}

// Sample is a batch of readings collected together.
type Sample []Reading

// CapInfo describes one capability’s retained info document.
type CapInfo struct {
	Kind string         // capability kind
	Info map[string]any // small JSONable map
}

// Adaptor owns a concrete device/driver and exposes generic hooks.
// Adaptors must NOT touch the bus or spawn goroutines.
type Adaptor interface {
	ID() string
	// Static capability descriptions (published as retained).
	Capabilities() []CapInfo
	// Trigger a measurement and return suggested wait until Collect.
	Trigger(ctx context.Context) (collectAfter time.Duration, err error)
	// Collect attempts to fetch a measurement batch; may return ErrNotReady.
	Collect(ctx context.Context) (Sample, error)
	// Optional pass-through control for driver-specific methods.
	// Return (nil, ErrUnsupported) if not implemented for a method/kind.
	Control(kind, method string, payload any) (result any, err error)
}

// WorkerConfig centralises timings and limits.
type WorkerConfig struct {
	TriggerTimeout time.Duration
	CollectTimeout time.Duration
	RetryBackoff   time.Duration
	MaxRetries     int
	InputQueueSize int
	ResultsQueueSz int
}

// MeasureReq asks the worker to trigger/collect for a given adaptor.
type MeasureReq struct {
	ID      string
	Adaptor Adaptor
	Prio    bool // true for read_now
}

// Result emitted by the worker.
type Result struct {
	ID     string
	Sample Sample
	Err    error
}

// ErrNotReady signals the worker to retry Collect after backoff.
var ErrNotReady = errNotReady{}

type errNotReady struct{}

func (errNotReady) Error() string { return "not ready" }

// ErrUnsupported for adaptor Control pass-through.
var ErrUnsupported = errUnsupported{}

type errUnsupported struct{}

func (errUnsupported) Error() string { return "unsupported" }

// I2CBusFactory injects configured I²C instances by id.
type I2CBusFactory interface {
	ByID(id string) (drivers.I2C, bool)
}

// ---- GPIO abstractions ----

type Pull uint8

const (
	PullNone Pull = iota
	PullUp
	PullDown
)

type GPIOPin interface {
	ConfigureInput(pull Pull) error
	ConfigureOutput(initial bool) error
	Set(level bool)
	Get() bool
	Toggle()
	Number() int
}

// Edge selection for IRQ.
type Edge uint8

const (
	EdgeNone Edge = iota
	EdgeRising
	EdgeFalling
	EdgeBoth
)

// IRQPin extends GPIOPin with interrupts.
type IRQPin interface {
	GPIOPin
	SetIRQ(edge Edge, handler func()) error
	ClearIRQ() error
}

// PinFactory supplies GPIO pins by the configured number scheme.
type PinFactory interface {
	ByNumber(n int) (GPIOPin, bool)
}

// GPIO IRQ configuration (optional per input).
type GPIOIRQ struct {
	Edge       string `json:"edge"`                  // "rising","falling","both","none"
	DebounceMS int    `json:"debounce_ms,omitempty"` // software debounce window
}

// GPIOParams config shape.
type GPIOParams struct {
	Pin     int      `json:"pin"`
	Mode    string   `json:"mode"`              // "input" | "output"
	Pull    string   `json:"pull,omitempty"`    // "up" | "down" | "none"
	Initial *bool    `json:"initial,omitempty"` // for outputs
	Invert  bool     `json:"invert,omitempty"`
	IRQ     *GPIOIRQ `json:"irq,omitempty"` // optional IRQ settings
}

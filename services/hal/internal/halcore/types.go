// services/hal/internal/halcore/types.go
package halcore

import (
	"context"
	"errors"
	"time"

	"tinygo.org/x/drivers"
)

// Reading is one datum for one capability kind.
type Reading struct {
	Kind    string // e.g. "temperature", "humidity", "voltage", "gpio"
	Payload any    // JSON-serialisable
	TsMs    int64  // producer timestamp (ms)
}

// Sample is a batch collected together.
type Sample []Reading

// CapInfo describes one capability’s retained info document.
type CapInfo struct {
	Kind string         // capability kind
	Info map[string]any // small JSONable map
}

// Adaptor abstracts a concrete device/driver. Must not own goroutines or the bus.
type Adaptor interface {
	ID() string
	Capabilities() []CapInfo
	// Split-phase measurement cycle.
	Trigger(ctx context.Context) (collectAfter time.Duration, err error)
	Collect(ctx context.Context) (Sample, error)
	// Optional pass-through control for device-specific methods.
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

// MeasureReq asks a worker to service an adaptor.
type MeasureReq struct {
	ID      string
	Adaptor Adaptor
	Prio    bool // true for "read_now"
}

// Result emitted by a worker.
type Result struct {
	ID     string
	Sample Sample
	Err    error
}

var (
	// ErrNotReady signals the worker to retry Collect after backoff.
	ErrNotReady = errors.New("not ready")
	// ErrUnsupported for adaptor Control pass-through.
	ErrUnsupported = errors.New("unsupported")
)

// ---- Buses ----

// I2CBusFactory injects configured I²C instances by id.
// Uses the TinyGo drivers.I2C interface to remain compatible on MCU builds.
type I2CBusFactory interface {
	ByID(id string) (drivers.I2C, bool)
}

// I2C is the subset we need (compatible with tinygo.org/x/drivers.I2C).
type I2C interface {
	Tx(addr uint16, w, r []byte) error
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

// Util
func EdgeToString(e Edge) string {
	switch e {
	case EdgeRising:
		return "rising"
	case EdgeFalling:
		return "falling"
	case EdgeBoth:
		return "both"
	default:
		return "none"
	}
}

// ---------------- UART abstractions ----------------

type UARTPort interface {
	// TX
	WriteByte(b byte) error
	Write(p []byte) (int, error)

	// RX
	Buffered() int
	Read(p []byte) (int, error)
	Readable() <-chan struct{}
	RecvSomeContext(ctx context.Context, p []byte) (int, error)
}

type UARTFactory interface {
	ByID(id string) (UARTPort, bool)
}

// Optional: formatting where platform supports it (no-op on host).
type UARTFormatter interface {
	SetBaudRate(br uint32)
	SetFormat(databits, stopbits uint8, parity uint8) error // parity: 0 none, 1 even, 2 odd
}

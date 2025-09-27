package core

import (
	"errors"
)

// ---- Bus taxonomy ----

type BusClass uint8

const (
	BusTransactional BusClass = iota // I²C, SPI, 1-Wire
	BusStream                        // UART, CAN, USB CDC
)

type ResourceID string // e.g. "i2c0", "uart0", "gpio25"

// ---- Transactional buses ----
// This repo standardises on a single *per-bus* worker goroutine that serialises
// all hardware access. Callers can either:
//   1) perform a direct synchronous transaction via Tx (blocks the caller), or
//   2) enqueue a job onto the bus worker with TryEnqueue (non-blocking control).

// I2CBus is the minimal surface a job needs while running on the worker.
type I2CBus interface {
	Tx(addr uint16, w []byte, r []byte) error
}

// I2COwner exposes both direct Tx and job enqueue.
// timeoutMS: 0 => provider default for direct Tx (if the provider supports one).
type I2COwner interface {
	Tx(addr uint16, w []byte, r []byte, timeoutMS int) error
	// TryEnqueue submits a job to the per-bus worker. It MUST be non-blocking:
	// returns false if the queue is saturated.
	TryEnqueue(job func(bus I2CBus) error) bool
}

// ---- Stream buses (independent RX/TX) ----

type StreamEvent struct {
	DevID string
	Data  []byte
	TSms  int64
}

type StreamStats struct {
	RxDrops uint32
	TxDrops uint32
	RxQLen  uint32
	TxQLen  uint32
}

type StreamOwner interface {
	TrySend(p []byte) bool      // non-blocking; false if queue full
	Events() <-chan StreamEvent // RX (and optional TX echo)
	Stats() StreamStats         // optional telemetry
}

// ---- GPIO handles ----

type Pull uint8

const (
	PullNone Pull = iota
	PullUp
	PullDown
)

type GPIOHandle interface {
	Number() int
	ConfigureInput(pull Pull) error
	ConfigureOutput(initial bool) error
	Set(bool)
	Get() bool
	Toggle()
}

// ---- Unified registry interface ----

type ResourceRegistry interface {
	// Optional classification/introspection.
	ClassOf(id ResourceID) (BusClass, bool)

	// Transactional buses
	ClaimI2C(devID string, id ResourceID) (I2COwner, error)
	ReleaseI2C(devID string, id ResourceID)

	// Stream buses
	ClaimStream(devID string, id ResourceID) (StreamOwner, error)
	ReleaseStream(devID string, id ResourceID)

	// GPIO
	ClaimGPIO(devID string, pin int) (GPIOHandle, error)
	ReleaseGPIO(devID string, pin int)
}

// Short error codes

var (
	ErrUnknownPin = errors.New("unknown_pin")
	ErrPinInUse   = errors.New("pin_in_use")

	ErrUnknownBus = errors.New("unknown_bus")
	ErrBusInUse   = errors.New("bus_in_use")
	ErrTimeout    = errors.New("timeout")
)

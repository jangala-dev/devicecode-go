package core

import (
	"time"

	"tinygo.org/x/drivers"
)

// ---- Bus taxonomy ----

type BusClass uint8

const (
	BusTransactional BusClass = iota // I2C, SPI, 1-Wire
	BusStream                        // UART, CAN, USB CDC
)

type ResourceID string // e.g. "i2c0", "uart0", "gpio25"

// ---- Unified pin-function model ----

type PinFunc uint8

const (
	FuncGPIOIn PinFunc = iota
	FuncGPIOOut
	FuncPWM
	// Extend here (e.g. FuncSPI_MOSI, FuncUART_TX, …) as we expose more functions.
)

// GPIO (function-specific view)
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

// ---- GPIO edge stream (provider-agnostic) ----
type GPIOEdge uint8

const (
	EdgeNone    GPIOEdge = 0
	EdgeRising  GPIOEdge = 1 << 0
	EdgeFalling GPIOEdge = 1 << 1
	EdgeBoth             = EdgeRising | EdgeFalling
)

// Provider-agnostic edge event; timestamp is from the provider worker.
type GPIOEdgeEvent struct {
	Pin   int   // GPIO number
	Level bool  // logic level after the edge
	TS    int64 // Unix ns
}

// Best-effort edge stream bound to a claimed input pin.
// Delivery must be non-blocking; dropping is permitted.
type GPIOEdgeStream interface {
	Events() <-chan GPIOEdgeEvent
	Close()
	SetDebounce(d time.Duration) bool
	SetEdges(sel GPIOEdge) bool
}

// PWM (function-specific view)
type PWMRampMode uint8

const (
	// Linear stepping: evenly spaced absolute steps from current to target.
	PWMRampLinear PWMRampMode = iota
	// Future modes could include gamma-corrected, exponential, or trapezoidal.
)

type PWMHandle interface {
	Configure(freqHz uint64, top uint16) error
	Set(level uint16)
	Enable(on bool)
	Info() (slice int, channel rune, pin int)

	Ramp(to uint16, durationMs uint32, steps uint16, mode PWMRampMode) bool
	StopRamp()
}

// PinHandle narrows to function-specific views; it is invalid to request a view
// that does not match the claimed function.
type PinHandle interface {
	Pin() int
	AsGPIO() GPIOHandle // only valid if claimed with FuncGPIOIn/FuncGPIOOut
	AsPWM() PWMHandle   // only valid if claimed with FuncPWM
}

// ---- Transactional buses (I²C) ----

// ---- Stream buses (shape reserved; provider can fill in) ----

// ---- Serial (UART et al.) ----
// Minimal, provider-agnostic serial data plane.
type SerialPort interface {
	// Non-blocking attempts (return 0 if nothing/nowhere).
	TryRead(p []byte) int
	TryWrite(p []byte) int

	// Coalesced readiness notifications (must re-check after wake).
	Readable() <-chan struct{}
	Writable() <-chan struct{}

	// Optional but useful for on-the-wire completion.
	Flush() error
}

type SerialConfigurator interface {
	SetBaudRate(br uint32) error
}

type SerialFormatConfigurator interface {
	// parity: "none" | "even" | "odd"
	SetFormat(databits, stopbits uint8, parity string) error
}

// ---- Unified registry interface ----

type ResourceRegistry interface {
	// Optional classification/introspection for controller-style resources.
	ClassOf(id ResourceID) (BusClass, bool)

	// Transactional buses (I²C): return a drivers.I2C view
	// that is safe to use concurrently and enforces per-bus serialisation.
	ClaimI2C(devID string, id ResourceID) (drivers.I2C, error)
	ReleaseI2C(devID string, id ResourceID)

	// Serial buses
	ClaimSerial(devID string, id ResourceID) (SerialPort, error)
	ReleaseSerial(devID string, id ResourceID)

	// Unified pin function claims
	ClaimPin(devID string, pin int, fn PinFunc) (PinHandle, error)
	ReleasePin(devID string, pin int)

	// GPIO edge subscriptions (exclusive per claimed input pin).
	SubscribeGPIOEdges(devID string, pin int, sel GPIOEdge, debounce time.Duration, buf int) (GPIOEdgeStream, error)
	UnsubscribeGPIOEdges(devID string, pin int)
}

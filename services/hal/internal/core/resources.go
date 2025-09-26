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

// ---- Transactional buses (serialised operations) ----

// I2COwner exposes a single atomic transaction.
// timeoutMS: 0 => provider default.
//
// The implementation serialises all hardware access behind a single worker
// per bus. Callers may invoke Tx from their own goroutines; Tx itself blocks
// until the transaction completes or times out.
type I2COwner interface {
	Tx(addr uint16, w []byte, r []byte, timeoutMS int) error
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

// ---- Device → HAL telemetry (single shape) ----
// By default, an Event represents a "value-like" update for a capability that
// HAL should publish to .../value (retained). If IsEvent is true, HAL instead
// publishes to .../event (non-retained). Err, when non-empty, causes HAL to
// publish only .../status=degraded (retained).

type Event struct {
	CapID    CapID  // target capability identity (assigned by HAL)
	Payload  any    // typed value payload (e.g. types.LEDValue)
	TSms     int64  // ms timestamp
	Err      string // "timeout","io_error","unsupported","unknown_pin",...
	IsEvent  bool   // true => publish to .../event (non-retained)
	EventTag string // optional subtopic tag for events (e.g. "rx","tx")
}

// ---- Event emission (devices → HAL) ----

type EventEmitter interface {
	// Emit tries to enqueue an Event for HAL publication.
	// It must be non-blocking; false indicates a drop under pressure.
	Emit(ev Event) bool
}

// ---- HAL-injected resources ----

type Resources struct {
	Reg ResourceRegistry
	Pub EventEmitter // provided by HAL; devices use it to emit values/events
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

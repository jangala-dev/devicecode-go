package core

import (
	"errors"

	"devicecode-go/types"
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

// ---- Owner → HAL event (single shape) ----

type Event struct {
	DevID   string     // logical device id (e.g. "led0")
	Kind    types.Kind // capability kind (e.g. KindLED)
	Payload any        // typed value payload (e.g. types.LEDValue)
	TSms    int64      // ms timestamp
	// Err when non-empty signals failure; HAL sets state:degraded and does not publish value.
	Err string // "timeout","io_error","unsupported","unknown_pin",...
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

	// Provider-owned GPIO ops that also emit events
	GPIOSet(devID string, pin int, level bool) (EnqueueResult, error)
	GPIOToggle(devID string, pin int) (EnqueueResult, error)
	GPIORead(devID string, pin int) (EnqueueResult, error)

	// Owners push values/errors here; HAL consumes and publishes.
	Events() <-chan Event
}

// Short error codes

var (
	ErrUnknownPin = errors.New("unknown_pin")
	ErrPinInUse   = errors.New("pin_in_use")

	ErrUnknownBus = errors.New("unknown_bus")
	ErrBusInUse   = errors.New("bus_in_use")
)

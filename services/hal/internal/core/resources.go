package core

import (
	"errors"

	"devicecode-go/types"
)

// ---- Bus taxonomy (reserved for future I²C/UART/etc.) ----

type BusClass uint8

const (
	BusTransactional BusClass = iota // I²C, SPI, 1-Wire
	BusStream                        // UART, CAN, USB CDC
)

type ResourceID string // e.g. "i2c0", "uart0", "gpio25"

// Transactional buses (serialised operations)
type TxnOwner interface {
	// Submit a transactional operation (to be defined when added).
	// For now kept as a placeholder to preserve the shape.
}

// Stream buses (independent RX/TX)
type StreamEvent struct {
	DevID string
	Data  []byte
	TSms  int64
}
type StreamOwner interface {
	// Submit/send non-blocking (to be defined when added).
	// Placeholder for future UART/CAN owners.
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
	// Err when non-empty signals operation failure; HAL sets state:degraded and does not publish value.
	Err string // "timeout","io_error","unsupported","unknown_pin",...
}

// ---- Unified registry interface ----

type ResourceRegistry interface {
	// Optional classification/introspection.
	ClassOf(id ResourceID) (BusClass, bool)

	// Exclusive claims (release on Device.Close). Owners filled in later for I²C/UART.
	ClaimTxn(devID string, id ResourceID, _ *struct{}) (TxnOwner, error)
	ClaimStream(devID string, id ResourceID, _ *struct{}) (StreamOwner, error)

	// GPIO (implemented on RP2040 provider).
	ClaimGPIO(devID string, pin int) (GPIOHandle, error)
	ReleaseTxn(devID string, id ResourceID)
	ReleaseStream(devID string, id ResourceID)
	ReleaseGPIO(devID string, pin int)

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

package core

import "errors"

// ---- Bus taxonomy (reserved for future I²C/UART/etc.) ----

type BusClass uint8

const (
	BusTransactional BusClass = iota // I²C, SPI, 1-Wire
	BusStream                        // UART, CAN, USB CDC
)

type ResourceID string // e.g. "i2c0", "uart0", "gpio25"

// Transactional buses (serialised closures)
type TxnOwner interface {
	Do(fn func() error) error
}

// Stream buses (independent RX/TX)
type StreamEvent struct {
	DevID string
	Data  []byte
	TSms  int64
}
type StreamOwner interface {
	TrySend(p []byte) bool
	Events() <-chan StreamEvent
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
	// Buses (placeholders for future extensions)
	ClassOf(id ResourceID) (BusClass, bool)
	Txn(id ResourceID) (TxnOwner, bool)
	Stream(id ResourceID) (StreamOwner, bool)

	// GPIO
	ClaimGPIO(devID string, pin int) (GPIOHandle, error)
	ReleaseGPIO(devID string, pin int)
}

// Short error codes
var (
	ErrUnknownPin = errors.New("unknown_pin")
	ErrPinInUse   = errors.New("pin_in_use")
)

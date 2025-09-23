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
	// Optional classification/introspection.
	ClassOf(id ResourceID) (BusClass, bool)

	// Exclusive claims (release on Device.Close). Providers for I²C/UART will fill these in later.
	ClaimTxn(devID string, id ResourceID, _ *struct{}) (TxnOwner, error)
	ClaimStream(devID string, id ResourceID, _ *struct{}) (StreamOwner, error)

	// GPIO (already implemented on RP2040).
	ClaimGPIO(devID string, pin int) (GPIOHandle, error)

	// Releases (idempotent; safe to call even if not claimed).
	ReleaseTxn(devID string, id ResourceID)
	ReleaseStream(devID string, id ResourceID)
	ReleaseGPIO(devID string, pin int)
}

// Short error codes
var (
	ErrUnknownPin = errors.New("unknown_pin")
	ErrPinInUse   = errors.New("pin_in_use")

	ErrUnknownBus = errors.New("unknown_bus")
	ErrBusInUse   = errors.New("bus_in_use")
)

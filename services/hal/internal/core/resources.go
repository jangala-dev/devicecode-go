package core

// ---- Bus taxonomy (unchanged) ----

type BusClass uint8

const (
	BusTransactional BusClass = iota // I²C, SPI, 1-Wire
	BusStream                        // UART, CAN, USB CDC
)

type ResourceID string // e.g. "i2c0", "uart0", "gpio25"

// ---- Unified pin-function model (new) ----

type PinFunc uint8

const (
	FuncGPIOIn PinFunc = iota
	FuncGPIOOut
	FuncPWM
	// Extend here (e.g. FuncSPI_MOSI, FuncUART_TX, …) as you expose more functions.
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

// PWM (function-specific view)
type PWMRampMode uint8

const (
	// Linear stepping: evenly spaced absolute steps from current to target.
	PWMRampLinear PWMRampMode = iota
	// Future modes could include gamma-corrected, exponential, or trapezoidal.
)

type PWMHandle interface {
	Configure(freqHz uint32, top uint16) error
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

// ---- Transactional buses ----
// Single *per-bus* worker goroutine that serialises hardware access.

type I2CBus interface {
	Tx(addr uint16, w []byte, r []byte) error
}

// I2COwner exposes both direct Tx and job enqueue.
// timeoutMS: 0 => provider default for direct Tx (if the provider supports one).
type I2COwner interface {
	Tx(addr uint16, w []byte, r []byte, timeoutMS int) error
	// TryEnqueue MUST be non-blocking: returns false if the queue is saturated.
	TryEnqueue(job func(bus I2CBus) error) bool
}

// ---- Stream buses (shape reserved; provider can fill in) ----

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

// ---- Unified registry interface (updated) ----

type ResourceRegistry interface {
	// Optional classification/introspection for controller-style resources.
	ClassOf(id ResourceID) (BusClass, bool)

	// Transactional buses
	ClaimI2C(devID string, id ResourceID) (I2COwner, error)
	ReleaseI2C(devID string, id ResourceID)

	// Stream buses
	ClaimStream(devID string, id ResourceID) (StreamOwner, error)
	ReleaseStream(devID string, id ResourceID)

	// Unified pin function claims
	ClaimPin(devID string, pin int, fn PinFunc) (PinHandle, error)
	ReleasePin(devID string, pin int)
}

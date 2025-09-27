package errcode

// Code is a stable, bus-facing error identifier.
// It is a string newtype, comparable, allocation-free, and implements error.
type Code string

func (c Code) Error() string { return string(c) }

// Canonical codes (short, stable).
const (
	OK                Code = "ok"
	Busy              Code = "busy"
	Unsupported       Code = "unsupported"
	InvalidParams     Code = "invalid_params"
	InvalidPayload    Code = "invalid_payload"
	UnknownCapability Code = "unknown_capability"
	HALNotReady       Code = "hal_not_ready"
	InvalidTopic      Code = "invalid_topic"

	UnknownBus Code = "unknown_bus"
	BusInUse   Code = "bus_in_use"
	UnknownPin Code = "unknown_pin"
	PinInUse   Code = "pin_in_use"
	Timeout    Code = "timeout"

	Error Code = "error" // generic fallback
)

// Optional wrapper when we want to keep context and a cause.
type E struct {
	C   Code
	Op  string
	Msg string
	Err error
}

func (e *E) Error() string {
	if e.Msg != "" {
		return string(e.C) + ": " + e.Msg
	}
	return string(e.C)
}
func (e *E) Unwrap() error { return e.Err }
func (e *E) Code() Code    { return e.C }

// Of extracts a Code from an error, defaulting to Error.
func Of(err error) Code {
	if err == nil {
		return OK
	}
	if c, ok := err.(Code); ok {
		return c
	}
	type coder interface{ Code() Code }
	if x, ok := err.(coder); ok {
		return x.Code()
	}
	return Error
}

// MapDriverErr maps low-level driver errors to a Code.
// Extend the heuristics per platform/driver.
func MapDriverErr(err error) Code {
	if err == nil {
		return OK
	}
	return Error
}

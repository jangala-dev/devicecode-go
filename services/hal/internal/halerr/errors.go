// services/hal/internal/halerr/errors.go
package halerr

import "errors"

var (
	// Service/control plane
	ErrBusy           = errors.New("busy")
	ErrInvalidPeriod  = errors.New("invalid_period")
	ErrInvalidCapAddr = errors.New("invalid_capability_address")
	ErrUnknownCap     = errors.New("unknown_capability")
	ErrNoAdaptor      = errors.New("no_adaptor")

	// Build/config
	ErrMissingBusRef = errors.New("missing_bus_ref")
	ErrUnknownBus    = errors.New("unknown_bus")
	ErrInvalidMode   = errors.New("invalid_mode")
	ErrUnknownPin    = errors.New("unknown_pin")

	// Generic / pass-through
	ErrUnsupported = errors.New("unsupported")
)

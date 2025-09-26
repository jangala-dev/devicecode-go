package core

import (
	"context"

	"devicecode-go/types"
)

// ---- Capability & device model ----

type CapabilitySpec struct {
	// Public addressing
	Domain string     // eg. "io","power","env"; if empty, HAL will infer a default
	Kind   types.Kind // capability kind (eg. KindLED, KindTemperature)
	Name   string     // logical instance name (role/location); if empty, HAL uses device ID

	Info  types.Info
	TTLms int // reserved for cache; 0 = none
}

// Enqueue-only control outcome returned by devices.
type EnqueueResult struct {
	OK    bool   // accepted/enqueued
	Error string // "busy","unsupported","invalid_payload","unknown_pin",...
}

// Device is device-centric: all controls are non-blocking and enqueue work
// at the relevant owner(s). Values are delivered later via owner events.
type Device interface {
	ID() string
	Capabilities() []CapabilitySpec
	Init(ctx context.Context) error
	Control(kind types.Kind, method string, payload any) (EnqueueResult, error)
	Close() error
}

// Builder input and registration

type BuilderInput struct {
	ID, Type string
	Params   any
	Res      Resources
}

type Builder interface {
	Build(ctx context.Context, in BuilderInput) (Device, error)
}

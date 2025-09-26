package core

import (
	"context"

	"devicecode-go/types"
)

// ---- Identity ---

// CapID is a compact, system-unique identifier for a single capability.
// It is assigned by the HAL when applying configuration and remains stable
// for the lifetime of that configuration.
type CapID uint32

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

// Device is device-centric: all controls are non-blocking and enqueue work.
// Values/events are emitted later by the device itself via the HAL emitter.
type Device interface {
	ID() string
	Capabilities() []CapabilitySpec
	// BindCapabilities is called exactly once after HAL assigns CapIDs.
	// The slice aligns positionally with Capabilities().
	BindCapabilities(ids []CapID)

	Init(ctx context.Context) error
	Control(cap CapID, method string, payload any) (EnqueueResult, error)
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

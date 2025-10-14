package core

import (
	"context"

	"devicecode-go/errcode"
	"devicecode-go/types"
)

// ---- Addressing ----

type CapAddr struct {
	Domain string     // e.g. "io","power","env"
	Kind   types.Kind // e.g. "led","temperature"
	Name   string     // logical instance
}

// ---- Capability & device model ----

type CapabilitySpec struct {
	Domain string
	Kind   types.Kind
	Name   string
	Info   types.Info
	TTLms  int // reserved; 0 = none
}

// Enqueue-only control outcome returned by devices.
type EnqueueResult struct {
	OK    bool
	Error errcode.Code // machine-readable short code
}

// Device is device-centric: controls are non-blocking.
type Device interface {
	ID() string
	Capabilities() []CapabilitySpec
	Init(ctx context.Context) error
	Control(cap CapAddr, method string, payload any) (EnqueueResult, error)
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

// ---- Device → HAL telemetry ----
// If Err != "", HAL publishes only status:degraded (retained).
// If IsEvent == true, publish non-retained event (optionally tagged) and still set status:up.
// Otherwise publish retained value and status:up.

type Event struct {
	Addr     CapAddr
	Payload  any
	TS       int64
	Err      string
	EventTag string
}

// ---- Event emission (devices → HAL) ----

type EventEmitter interface {
	// Emit publishes the event best-effort; it MUST NOT block the device
	// for long-running operations (in this variant it forwards synchronously).
	Emit(ev Event) bool
}

// ---- HAL-injected resources ----

type Resources struct {
	Reg ResourceRegistry
	Pub EventEmitter
}

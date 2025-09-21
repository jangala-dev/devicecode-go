package core

import (
	"context"
	"devicecode-go/types"
)

// ---- Capability & device model ----

type CapabilitySpec struct {
	Kind  types.Kind
	Info  types.Info
	TTLms int // reserved for cache; 0 = none
}

type Device interface {
	ID() string
	Capabilities() []CapabilitySpec
	Init(ctx context.Context) error
	Read(ctx context.Context, emit func(kind types.Kind, payload any)) error
	Control(kind types.Kind, method string, payload any) (any, error)
	Close() error // optional: release claimed resources
}

// ---- HAL-injected resources ----

type Resources struct {
	Reg ResourceRegistry
}

// Builder input
type BuilderInput struct {
	ID, Type string
	Params   any
	Res      Resources
}

type Builder interface {
	Build(ctx context.Context, in BuilderInput) (Device, error)
}

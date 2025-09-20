package core

import (
	"context"

	"devicecode-go/types"
)

// ---- Capability & device model (lean) ----

type CapabilitySpec struct {
	Kind  types.Kind
	Info  types.Info
	TTLms int // reserved for future cache; 0 = none
}

type Device interface {
	ID() string
	Capabilities() []CapabilitySpec
	Init(ctx context.Context) error
	Read(ctx context.Context, emit func(kind types.Kind, payload any)) error
	Control(kind types.Kind, method string, payload any) (any, error)
}

// ---- Minimal platform shim (GPIO only for LED bring-up) ----

type Pull uint8

const (
	PullNone Pull = iota
	PullUp
	PullDown
)

type GPIOPin interface {
	ConfigureInput(pull Pull) error
	ConfigureOutput(initial bool) error
	Set(bool)
	Get() bool
	Toggle()
	Number() int
}

type PinFactory interface {
	ByNumber(n int) (GPIOPin, bool)
}

type Resources struct {
	Pins PinFactory
}

// Builder input (exported types used at boundary)
type BuilderInput struct {
	ID, Type string
	Params   any
	Pins     PinFactory
}

type Builder interface {
	Build(ctx context.Context, in BuilderInput) (Device, error)
}

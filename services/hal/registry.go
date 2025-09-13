// services/hal/registry.go
package hal

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// BuildInput is provided to a device builder to construct an Adaptor and any
// ancillary requirements (bus usage, IRQ, sampling policy).
type BuildInput struct {
	Ctx        context.Context
	Buses      I2CBusFactory
	Pins       PinFactory
	DeviceID   string
	Type       string
	ParamsJSON any
	// Minimal BusRef shape; mirrors your config without pulling it in here.
	BusRef struct {
		Type string
		ID   string
	}
}

// BuildOutput is returned by a builder.
type BuildOutput struct {
	Adaptor     Adaptor
	BusID       string        // optional: bucket key for a shared worker (e.g. "i2c0")
	SampleEvery time.Duration // 0 if not a periodic producer
	IRQ         *IRQRequest   // nil if the device has no IRQ needs
}

// IRQRequest describes a GPIO IRQ subscription to be registered by the service.
type IRQRequest struct {
	DevID      string
	Pin        IRQPin
	Edge       Edge
	DebounceMS int
	Invert     bool
}

// Builder constructs an Adaptor from config and platform factories.
type Builder interface {
	Build(in BuildInput) (BuildOutput, error)
}

var (
	muBuilders sync.RWMutex
	builders   = map[string]Builder{}
)

// RegisterBuilder installs a builder for a given device type string.
// It panics on duplicate registration to catch mistakes at start-up.
func RegisterBuilder(deviceType string, b Builder) {
	muBuilders.Lock()
	defer muBuilders.Unlock()
	if deviceType == "" {
		panic("hal: empty device type for builder")
	}
	if _, exists := builders[deviceType]; exists {
		panic(fmt.Sprintf("hal: builder already registered for type %q", deviceType))
	}
	builders[deviceType] = b
}

// findBuilder looks up a registered builder by type.
func findBuilder(deviceType string) (Builder, bool) {
	muBuilders.RLock()
	defer muBuilders.RUnlock()
	b, ok := builders[deviceType]
	return b, ok
}

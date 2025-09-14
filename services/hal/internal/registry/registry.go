// services/hal/internal/registry/registry.go
package registry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"devicecode-go/services/hal/internal/halcore"
)

// BuildInput is passed to a device builder.
type BuildInput struct {
	Ctx        context.Context
	Buses      halcore.I2CBusFactory
	Pins       halcore.PinFactory
	DeviceID   string
	Type       string
	ParamsJSON interface{}
	BusRefType string // e.g. "i2c"
	BusRefID   string // e.g. "i2c0"
}

// BuildOutput describes a constructed device.
type BuildOutput struct {
	Adaptor     halcore.Adaptor
	BusID       string        // "" if not on a shared bus
	SampleEvery time.Duration // 0 if not a periodic producer
	IRQ         *IRQRequest   // nil if none
}

// IRQRequest asks the service to register a GPIO IRQ.
type IRQRequest struct {
	DevID      string
	Pin        halcore.IRQPin
	Edge       halcore.Edge
	DebounceMS int
	Invert     bool
}

// Builder creates an adaptor from config and factories.
type Builder interface {
	Build(in BuildInput) (BuildOutput, error)
}

var (
	mu       sync.RWMutex
	builders = map[string]Builder{}
)

func RegisterBuilder(deviceType string, b Builder) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := builders[deviceType]; exists {
		panic(fmt.Sprintf("device builder already registered for type %q", deviceType))
	}
	builders[deviceType] = b
}

func Lookup(deviceType string) (Builder, bool) {
	mu.RLock()
	defer mu.RUnlock()
	b, ok := builders[deviceType]
	return b, ok
}

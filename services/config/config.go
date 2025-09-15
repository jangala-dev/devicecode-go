package config

import (
	"context"
	"errors"

	"devicecode-go/bus"

	"github.com/andreyvit/tinyjson"
)

// -----------------------------------------------------------------------------
// String constants (live in flash, not RAM)
// -----------------------------------------------------------------------------

const (
	serviceName  = "config"
	configPrefix = "config"
	CtxDeviceKey = "device" // context key used for device ID
)

// EmbeddedConfigLookup allows overriding how configs are resolved.
var EmbeddedConfigLookup = func(device string) ([]byte, bool) {
	b, ok := embeddedConfigs[device]
	return b, ok
}

// -----------------------------------------------------------------------------
// Config Service
// -----------------------------------------------------------------------------

type ConfigService struct {
	Name string
}

func NewConfigService() *ConfigService {
	return &ConfigService{Name: serviceName}
}

// publishConfig reads the device config from embedded data and publishes it as retained messages.
func (s *ConfigService) publishConfig(ctx context.Context, conn *bus.Connection) error {
	device, _ := ctx.Value(CtxDeviceKey).(string)
	if device == "" {
		return errors.New("missing device ID in context")
	}

	raw, ok := EmbeddedConfigLookup(device)
	if !ok || len(raw) == 0 {
		return errors.New("no embedded config for device: " + device)
	}

	r := tinyjson.Raw(raw)
	val := r.Value() // should be a map[string]any
	r.EnsureEOF()

	m, ok := val.(map[string]any)
	if !ok {
		return errors.New("embedded config is not a JSON object")
	}

	for k, v := range m {
		msg := &bus.Message{
			Topic:    bus.T(configPrefix, k),
			Payload:  v,
			Retained: true,
		}
		conn.Publish(msg)
	}

	return nil
}

// Start launches the config publisher in a goroutine.
func (s *ConfigService) Start(ctx context.Context, conn *bus.Connection) {
	go func() {
		_ = s.publishConfig(ctx, conn) // replace with logging if needed
	}()
}

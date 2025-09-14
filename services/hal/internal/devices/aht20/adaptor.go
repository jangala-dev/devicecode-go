// services/hal/internal/devices/aht20/adaptor.go
package aht20

import (
	"context"
	"time"

	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/services/hal/internal/util"
)

// Register this device type with the registry.
func init() {
	registry.RegisterBuilder("aht20", aht20Builder{})
}

type aht20Builder struct{}

func (aht20Builder) Build(in registry.BuildInput) (registry.BuildOutput, error) {
	if in.BusRefType != "i2c" || in.BusRefID == "" {
		return registry.BuildOutput{}, util.Errf("missing i2c bus")
	}
	i2c, ok := in.Buses.ByID(in.BusRefID)
	if !ok {
		return registry.BuildOutput{}, util.Errf("unknown bus %q", in.BusRefID)
	}
	// Params: { "addr": 0x38 }
	var p struct {
		Addr int `json:"addr"`
	}
	_ = util.DecodeJSON(in.ParamsJSON, &p)
	if p.Addr == 0 {
		p.Addr = 0x38
	}
	dev := newAHT20(i2c, uint16(p.Addr)) // platform-specific
	ad := &adaptor{id: in.DeviceID, dev: dev}
	ad.configure(uint16(p.Addr))
	return registry.BuildOutput{
		Adaptor:     ad,
		BusID:       in.BusRefID,
		SampleEvery: 2 * time.Second,
	}, nil
}

type adaptor struct {
	id  string
	dev aht20Device
}

func (a *adaptor) ID() string { return a.id }

func (a *adaptor) Capabilities() []halcore.CapInfo {
	return []halcore.CapInfo{
		{Kind: "temperature", Info: map[string]any{"unit": "C", "precision": 0.1, "schema_version": 1, "driver": "aht20"}},
		{Kind: "humidity", Info: map[string]any{"unit": "%RH", "precision": 0.1, "schema_version": 1, "driver": "aht20"}},
	}
}

func (a *adaptor) Trigger(ctx context.Context) (time.Duration, error) {
	if err := a.dev.Trigger(); err != nil {
		return 0, err
	}
	return a.dev.TriggerHint(), nil
}

func (a *adaptor) Collect(ctx context.Context) (halcore.Sample, error) {
	var s aht20Sample
	if err := a.dev.Collect(&s); err != nil {
		if err == errNotReady {
			return nil, halcore.ErrNotReady
		}
		return nil, err
	}
	ts := time.Now().UnixMilli()
	return halcore.Sample{
		{Kind: "temperature", Payload: map[string]any{"deci_c": s.DeciCelsius(), "ts_ms": ts}, TsMs: ts},
		{Kind: "humidity", Payload: map[string]any{"deci_percent": s.DeciRelHumidity(), "ts_ms": ts}, TsMs: ts},
	}, nil
}

func (a *adaptor) Control(kind, method string, payload any) (any, error) {
	// No device-specific controls in this pass.
	return nil, halcore.ErrUnsupported
}

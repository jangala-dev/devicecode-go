// services/hal/adaptor_aht20_driver.go
package hal

import (
	"context"
	"time"

	"devicecode-go/drivers/aht20"

	"tinygo.org/x/drivers"
)

type aht20Adaptor struct {
	id  string
	dev aht20.Device
}

func NewAHT20Adaptor(id string, bus drivers.I2C, addr uint16) Adaptor {
	if addr == 0 {
		addr = aht20.Address
	}
	dev := aht20.New(bus)
	dev.Configure(aht20.Config{
		Address:        addr,
		PollInterval:   15 * time.Millisecond,
		CollectTimeout: 250 * time.Millisecond,
		TriggerHint:    80 * time.Millisecond,
	})
	return &aht20Adaptor{id: id, dev: dev}
}

func (a *aht20Adaptor) ID() string { return a.id }

func (a *aht20Adaptor) Capabilities() []CapInfo {
	return []CapInfo{
		{Kind: "temperature", Info: map[string]any{"unit": "C", "precision": 0.1, "schema_version": 1, "driver": "aht20"}},
		{Kind: "humidity", Info: map[string]any{"unit": "%RH", "precision": 0.1, "schema_version": 1, "driver": "aht20"}},
	}
}

func (a *aht20Adaptor) Trigger(ctx context.Context) (time.Duration, error) {
	if err := a.dev.Trigger(); err != nil {
		return 0, err
	}
	return a.dev.TriggerHint(), nil
}

func (a *aht20Adaptor) Collect(ctx context.Context) (Sample, error) {
	var s aht20.Sample
	if err := a.dev.Collect(&s); err != nil {
		if err == aht20.ErrNotReady {
			return nil, ErrNotReady
		}
		return nil, err
	}
	ts := time.Now().UnixMilli()
	return Sample{
		{Kind: "temperature", Payload: map[string]any{"deci_c": s.DeciCelsius(), "ts_ms": ts}, TsMs: ts},
		{Kind: "humidity", Payload: map[string]any{"deci_percent": s.DeciRelHumidity(), "ts_ms": ts}, TsMs: ts},
	}, nil
}

func (a *aht20Adaptor) Control(kind, method string, payload any) (any, error) {
	// No device-specific controls for AHT20 in this pass.
	return nil, ErrUnsupported
}

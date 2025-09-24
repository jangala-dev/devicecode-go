package led

import (
	"context"
	"errors"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

func init() { core.RegisterBuilder("gpio_led", builder{}) }

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	var p types.LEDParams
	switch v := in.Params.(type) {
	case types.LEDParams:
		p = v
	case *types.LEDParams:
		p = *v
	default:
		return nil, errors.New("invalid_params_type")
	}
	if p.Pin < 0 {
		return nil, errors.New("invalid_or_missing_pin")
	}
	h, err := in.Res.Reg.ClaimGPIO(in.ID, p.Pin)
	if err != nil {
		return nil, err
	}
	return &Device{
		id: in.ID, pin: h, pinN: p.Pin,
		reg:     in.Res.Reg, // depend only on the stable registry
		initial: p.Initial,
	}, nil
}

type Device struct {
	id      string
	pin     core.GPIOHandle
	pinN    int
	reg     core.ResourceRegistry
	initial bool
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{{
		Kind: types.KindLED,
		Info: types.Info{
			SchemaVersion: 1,
			Driver:        "gpio_led",
			Detail:        types.LEDInfo{Pin: d.pin.Number()},
		},
	}}
}

func (d *Device) Init(ctx context.Context) error {
	return d.pin.ConfigureOutput(d.initial)
}

// Control is enqueue-only and immediate. Values are emitted via registry events.
func (d *Device) Control(kind types.Kind, method string, payload any) (core.EnqueueResult, error) {
	if kind != types.KindLED {
		return core.EnqueueResult{OK: false, Error: "unsupported"}, nil
	}
	switch method {
	case "set":
		var lvl bool
		switch p := payload.(type) {
		case types.LEDSet:
			lvl = p.Level
		case map[string]any:
			b, ok := p["level"].(bool)
			if !ok {
				return core.EnqueueResult{OK: false, Error: "invalid_payload"}, nil
			}
			lvl = b
		default:
			return core.EnqueueResult{OK: false, Error: "invalid_payload"}, nil
		}
		return d.reg.GPIOSet(d.id, d.pinN, lvl)

	case "toggle":
		return d.reg.GPIOToggle(d.id, d.pinN)

	case "read":
		return d.reg.GPIORead(d.id, d.pinN)

	default:
		return core.EnqueueResult{OK: false, Error: "unsupported"}, nil
	}
}

func (d *Device) Close() error {
	// Optionally release on reconfig when implemented:
	// d.reg.ReleaseGPIO(d.id, d.pinN)
	return nil
}

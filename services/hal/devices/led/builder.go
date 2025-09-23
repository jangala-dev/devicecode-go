package led

import (
	"context"
	"errors"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

func init() { core.RegisterBuilder("gpio_led", builder{}) }

type builder struct{}

// gpioOps is a narrow interface the registry implements on RP2040.
// This keeps the device decoupled from the concrete provider type.
type gpioOps interface {
	GPIOSet(devID string, pin int, level bool) (core.EnqueueResult, error)
	GPIOToggle(devID string, pin int) (core.EnqueueResult, error)
	GPIORead(devID string, pin int) (core.EnqueueResult, error)
}

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
	ops, ok := in.Res.Reg.(gpioOps)
	if !ok {
		return nil, errors.New("gpio_ops_unavailable")
	}
	return &Device{id: in.ID, pin: h, pinN: p.Pin, ops: ops, initial: p.Initial}, nil
}

type Device struct {
	id      string
	pin     core.GPIOHandle
	pinN    int
	ops     gpioOps
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
		case nil:
			// treat missing payload as invalid
			return core.EnqueueResult{OK: false, Error: "invalid_payload"}, nil
		default:
			return core.EnqueueResult{OK: false, Error: "invalid_payload"}, nil
		}
		return d.ops.GPIOSet(d.id, d.pinN, lvl)

	case "toggle":
		return d.ops.GPIOToggle(d.id, d.pinN)

	case "read":
		return d.ops.GPIORead(d.id, d.pinN)

	default:
		return core.EnqueueResult{OK: false, Error: "unsupported"}, nil
	}
}

func (d *Device) Close() error {
	// Optionally release on reconfig when implemented:
	// in.Res.Reg.ReleaseGPIO(d.id, d.pinN)
	return nil
}

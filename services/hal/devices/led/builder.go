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
	return &Device{id: in.ID, pin: h, initial: p.Initial}, nil
}

type Device struct {
	id      string
	pin     core.GPIOHandle
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

func (d *Device) Read(ctx context.Context, emit func(k types.Kind, payload any)) error {
	l := d.pin.Get()
	var lvl uint8
	if l {
		lvl = 1
	}
	emit(types.KindLED, types.LEDValue{Level: lvl})
	return nil
}

func (d *Device) Control(kind types.Kind, method string, payload any) (any, error) {
	if kind != types.KindLED {
		return nil, errors.New("unsupported_kind")
	}
	switch method {
	case "set":
		// Accept either typed or map payloads.
		var lvl bool
		if p, ok := payload.(types.LEDSet); ok {
			lvl = p.Level
		} else if m, ok := payload.(map[string]any); ok {
			b, ok2 := m["level"].(bool)
			if !ok2 {
				return nil, errors.New("invalid_payload")
			}
			lvl = b
		} else {
			return nil, errors.New("invalid_payload")
		}
		d.pin.Set(lvl)
		var v uint8
		if lvl {
			v = 1
		}
		return types.LEDValue{Level: v}, nil

	case "toggle":
		d.pin.Toggle()
		var v uint8
		if d.pin.Get() {
			v = 1
		}
		return types.LEDValue{Level: v}, nil

	default:
		return nil, errors.New("unsupported_method")
	}
}

func (d *Device) Close() error {
	// Optional: release claim (safe if registry is retained between configs)
	// in.Res.Reg.ReleaseGPIO(d.id, d.pin.Number())  // not accessible here; skip for now
	return nil
}

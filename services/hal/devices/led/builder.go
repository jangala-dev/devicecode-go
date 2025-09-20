package led

import (
	"context"
	"errors"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

func init() { core.RegisterBuilder("gpio_led", builder{}) }

type Params struct {
	Pin     int  `json:"pin"`
	Initial bool `json:"initial,omitempty"`
}

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	var p Params
	if m, ok := in.Params.(map[string]any); ok {
		if v, ok := m["pin"].(float64); ok {
			p.Pin = int(v)
		}
		if v, ok := m["initial"].(bool); ok {
			p.Initial = v
		}
	}
	if p.Pin == 0 && p.Initial { /* acceptable */
	}
	pin, ok := in.Pins.ByNumber(p.Pin)
	if !ok {
		return nil, errors.New("unknown_pin")
	}
	return &Device{id: in.ID, pin: pin, initial: p.Initial}, nil
}

// Device implements a single LED capability on a GPIO.
type Device struct {
	id      string
	pin     core.GPIOPin
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
	emit(types.KindLED, types.LEDValue{Level: func() uint8 {
		if l {
			return 1
		}
		return 0
	}()})
	return nil
}

func (d *Device) Control(kind types.Kind, method string, payload any) (any, error) {
	if kind != types.KindLED {
		return nil, errors.New("unsupported_kind")
	}
	switch method {
	case "set":
		m, ok := payload.(map[string]any)
		if !ok {
			return nil, errors.New("invalid_payload")
		}
		b, ok := m["level"].(bool)
		if !ok {
			return nil, errors.New("invalid_payload")
		}
		d.pin.Set(b)
		return types.OKReply{OK: true}, nil
	case "toggle":
		d.pin.Toggle()
		return types.OKReply{OK: true}, nil
	default:
		return nil, errors.New("unsupported_method")
	}
}

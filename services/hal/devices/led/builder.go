package led

import (
	"context"
	"errors"
	"time"

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
		reg:     in.Res.Reg, // stable registry
		pub:     in.Res.Pub, // HAL emitter
		initial: p.Initial,
	}, nil
}

type Device struct {
	id      string
	pin     core.GPIOHandle
	pinN    int
	reg     core.ResourceRegistry
	initial bool
	pub     core.EventEmitter
	capID   core.CapID
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{{
		Domain: "io",
		Kind:   types.KindLED,
		Name:   d.id, // keep simple; you may later allow an override in params
		Info: types.Info{
			SchemaVersion: 1,
			Driver:        "gpio_led",
			Detail:        types.LEDInfo{Pin: d.pin.Number()},
		},
	}}
}

func (d *Device) BindCapabilities(ids []core.CapID) {
	if len(ids) > 0 {
		d.capID = ids[0]
	}
}

func (d *Device) Init(ctx context.Context) error {
	return d.pin.ConfigureOutput(d.initial)
}

// services/hal/devices/led/builder.go
func (d *Device) Control(_ core.CapID, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "set":
		p, ok := payload.(types.LEDSet)
		if !ok {
			return core.EnqueueResult{OK: false, Error: "invalid_payload"}, nil
		}
		d.pin.Set(p.Level)
		d.emitValueNow()
		return core.EnqueueResult{OK: true}, nil
	case "toggle":
		d.pin.Toggle()
		d.emitValueNow()
		return core.EnqueueResult{OK: true}, nil
	case "read":
		d.emitValueNow()
		return core.EnqueueResult{OK: true}, nil
	default:
		return core.EnqueueResult{OK: false, Error: "unsupported"}, nil
	}
}

func (d *Device) emitValueNow() {
	var v uint8
	if d.pin.Get() {
		v = 1
	}
	_ = d.pub.Emit(core.Event{
		CapID:   d.capID,
		Payload: types.LEDValue{Level: v},
		TSms:    timeNowMs(),
	})
}

func timeNowMs() int64 { return time.Now().UnixMilli() }

func (d *Device) Close() error {
	// Optionally release on reconfig when implemented:
	// d.reg.ReleaseGPIO(d.id, d.pinN)
	return nil
}

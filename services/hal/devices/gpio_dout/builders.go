package gpio_dout

import (
	"context"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
)

func init() {
	core.RegisterBuilder("gpio_led", builderLED{})
	core.RegisterBuilder("gpio_switch", builderSwitch{})
}

type builderLED struct{}
type builderSwitch struct{}

func (builderLED) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, err := parseParams(in.Params)
	if err != nil {
		return nil, err
	}
	ph, err := in.Res.Reg.ClaimPin(in.ID, p.Pin, core.FuncGPIOOut)
	if err != nil {
		return nil, err
	}
	gpio := ph.AsGPIO()
	return New(RoleLED, in.ID, p, gpio, in.Res.Pub), nil
}

func (builderSwitch) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, err := parseParams(in.Params)
	if err != nil {
		return nil, err
	}
	if p.Domain == "" {
		p.Domain = "power"
	}
	ph, err := in.Res.Reg.ClaimPin(in.ID, p.Pin, core.FuncGPIOOut)
	if err != nil {
		return nil, err
	}
	gpio := ph.AsGPIO()
	return New(RoleSwitch, in.ID, p, gpio, in.Res.Pub), nil
}

func parseParams(v any) (Params, error) {
	switch p := v.(type) {
	case Params:
		return p, nil
	case *Params:
		return *p, nil
	default:
		return Params{}, errcode.InvalidParams
	}
}

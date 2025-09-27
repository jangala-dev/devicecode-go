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
	h, err := in.Res.Reg.ClaimGPIO(in.ID, p.Pin)
	if err != nil {
		return nil, err
	}
	return New(RoleLED, in.ID, p, h, in.Res.Pub), nil
}

func (builderSwitch) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, err := parseParams(in.Params)
	if err != nil {
		return nil, err
	}
	if p.Domain == "" {
		p.Domain = "power"
	}
	h, err := in.Res.Reg.ClaimGPIO(in.ID, p.Pin)
	if err != nil {
		return nil, err
	}
	return New(RoleSwitch, in.ID, p, h, in.Res.Pub), nil
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

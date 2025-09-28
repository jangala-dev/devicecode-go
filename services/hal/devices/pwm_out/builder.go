// services/hal/devices/pwm_out/builder.go
package pwm_out

import (
	"context"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/x/strx"
)

func init() { core.RegisterBuilder("pwm_out", builder{}) }

type Params struct {
	Pin    int
	FreqHz uint64 // desired frequency
	Top    uint16 // wrap value
	Domain string
	Name   string
}

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok || p.Pin < 0 {
		return nil, errcode.InvalidParams
	}
	ph, err := in.Res.Reg.ClaimPin(in.ID, p.Pin, core.FuncPWM)
	if err != nil {
		return nil, err
	}
	pwm := ph.AsPWM()
	dev := &Device{
		id:   in.ID,
		pin:  p.Pin,
		pwm:  pwm,
		pub:  in.Res.Pub,
		dom:  strx.Coalesce(p.Domain, "io"),
		name: strx.Coalesce(p.Name, in.ID),
		freq: p.FreqHz,
		top:  p.Top,
	}
	return dev, nil
}

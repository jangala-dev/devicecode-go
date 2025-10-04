package gpio_dout

import (
	"context"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
)

func init() {
	// Register both device kinds with a single parameterised builder.
	core.RegisterBuilder("gpio_led", gpioBuilder{role: RoleLED})
	core.RegisterBuilder("gpio_switch", gpioBuilder{role: RoleSwitch})
}

// One builder, parameterised by device role.
type gpioBuilder struct{ role Role }

func (b gpioBuilder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, err := parseParams(in.Params)
	if err != nil {
		return nil, err
	}
	// Enforce explicit addressing.
	if p.Domain == "" || p.Name == "" {
		return nil, errcode.InvalidParams
	}
	ph, err := in.Res.Reg.ClaimPin(in.ID, p.Pin, core.FuncGPIOOut)
	if err != nil {
		return nil, err
	}
	gpio := ph.AsGPIO()

	// Note: Device.New applies sensible defaults:
	//  - RoleSwitch => domain "power" if empty
	//  - RoleLED    => domain "io"    if empty
	return New(b.role, in.ID, p, gpio, in.Res.Pub, in.Res.Reg), nil
}

// Parameter parsing retained as-is.
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

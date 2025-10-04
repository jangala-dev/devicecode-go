package gpio_button

import (
	"context"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
)

func init() { core.RegisterBuilder("gpio_button", builder{}) }

type Params struct {
	Pin        int
	Pull       string // "none","up","down"
	Invert     bool   // true if pressed == low
	DebounceMs uint16
	Domain     string
	Name       string
}

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok || p.Pin < 0 {
		return nil, errcode.InvalidParams
	}
	if p.Domain == "" || p.Name == "" {
		return nil, errcode.InvalidParams
	}

	ph, err := in.Res.Reg.ClaimPin(in.ID, p.Pin, core.FuncGPIOIn)
	if err != nil {
		return nil, err
	}
	gpio := ph.AsGPIO()
	switch p.Pull {
	case "up":
		_ = gpio.ConfigureInput(core.PullUp)
	case "down":
		_ = gpio.ConfigureInput(core.PullDown)
	default:
		_ = gpio.ConfigureInput(core.PullNone)
	}
	debounce := time.Duration(p.DebounceMs) * time.Millisecond

	return &Device{
		id:       in.ID,
		pinN:     p.Pin,
		gpio:     gpio,
		invert:   p.Invert,
		pub:      in.Res.Pub,
		reg:      in.Res.Reg,
		dom:      p.Domain,
		name:     p.Name,
		debounce: debounce,
	}, nil
}

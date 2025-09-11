// services/hal/adaptor_gpio.go
package hal

import (
	"context"
	"errors"
	"time"
)

type gpioAdaptor struct {
	id     string
	pin    GPIOPin
	params GPIOParams
}

func NewGPIOAdaptor(id string, pin GPIOPin, p GPIOParams) Adaptor {
	return &gpioAdaptor{id: id, pin: pin, params: p}
}

func (a *gpioAdaptor) ID() string { return a.id }

func (a *gpioAdaptor) Capabilities() []CapInfo {
	mode := a.params.Mode
	if mode != "input" && mode != "output" {
		mode = "output"
	}
	info := map[string]any{
		"pin":            a.pin.Number(),
		"mode":           mode,
		"invert":         a.params.Invert,
		"pull":           a.params.Pull,
		"schema_version": 1,
	}
	return []CapInfo{{Kind: "gpio", Info: info}}
}

// GPIO adaptor is control-only; sampling is done via IRQ worker.
func (a *gpioAdaptor) Trigger(context.Context) (time.Duration, error) {
	return 0, errors.New("unsupported")
}
func (a *gpioAdaptor) Collect(context.Context) (Sample, error) { return nil, errors.New("unsupported") }

func (a *gpioAdaptor) Control(kind, method string, payload any) (any, error) {
	if kind != "gpio" {
		return nil, ErrUnsupported
	}
	switch method {
	case "configure_input":
		return a.confInput(payload)
	case "configure_output":
		return a.confOutput(payload)
	case "set":
		lvl := wantBool(payload, "level")
		if a.params.Invert {
			lvl = !lvl
		}
		a.pin.Set(lvl)
		return map[string]any{"ok": true}, nil
	case "get":
		lvl := a.pin.Get()
		if a.params.Invert {
			lvl = !lvl
		}
		return map[string]any{"level": boolToInt(lvl)}, nil
	case "toggle":
		if t, ok := any(a.pin).(interface{ Toggle() }); ok && t != nil {
			t.Toggle()
		} else {
			cur := a.pin.Get()
			a.pin.Set(!cur)
		}
		return map[string]any{"ok": true}, nil
	default:
		return nil, ErrUnsupported
	}
}

func (a *gpioAdaptor) confInput(p any) (any, error) {
	pl := mapFromAny(p)
	pull := parsePull(pl["pull"])
	if err := a.pin.ConfigureInput(pull); err != nil {
		return nil, err
	}
	a.params.Mode = "input"
	a.params.Pull = toPullString(pull)
	return map[string]any{"mode": "input", "pull": a.params.Pull}, nil
}

func (a *gpioAdaptor) confOutput(p any) (any, error) {
	pl := mapFromAny(p)
	init := wantBool(pl, "initial")
	if a.params.Invert {
		init = !init
	}
	if err := a.pin.ConfigureOutput(init); err != nil {
		return nil, err
	}
	a.params.Mode = "output"
	a.params.Initial = &init
	return map[string]any{"mode": "output"}, nil
}

// helpers

func mapFromAny(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

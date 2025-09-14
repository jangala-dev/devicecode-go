// services/hal/internal/devices/gpio/adaptor.go
package gpio

import (
	"context"
	"errors"
	"time"

	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/services/hal/internal/util"
)

func init() {
	registry.RegisterBuilder("gpio", gpioBuilder{})
}

type GPIOIRQ struct {
	Edge       string `json:"edge"`                  // "rising","falling","both","none"
	DebounceMS int    `json:"debounce_ms,omitempty"` // software debounce window
}

type Params struct {
	Pin     int      `json:"pin"`
	Mode    string   `json:"mode"`              // "input" | "output"
	Pull    string   `json:"pull,omitempty"`    // "up" | "down" | "none"
	Initial *bool    `json:"initial,omitempty"` // for outputs
	Invert  bool     `json:"invert,omitempty"`
	IRQ     *GPIOIRQ `json:"irq,omitempty"` // optional IRQ settings
}

type gpioBuilder struct{}

func (gpioBuilder) Build(in registry.BuildInput) (registry.BuildOutput, error) {
	var p Params
	if err := util.DecodeJSON(in.ParamsJSON, &p); err != nil {
		return registry.BuildOutput{}, err
	}
	pin, ok := in.Pins.ByNumber(p.Pin)
	if !ok {
		return registry.BuildOutput{}, util.Errf("unknown pin %d", p.Pin)
	}

	// Configure initial mode.
	switch p.Mode {
	case "input":
		if err := pin.ConfigureInput(parsePull(p.Pull)); err != nil {
			return registry.BuildOutput{}, err
		}
	case "output":
		init := false
		if p.Initial != nil {
			init = *p.Initial
		}
		if p.Invert {
			init = !init
		}
		if err := pin.ConfigureOutput(init); err != nil {
			return registry.BuildOutput{}, err
		}
	default:
		return registry.BuildOutput{}, util.Errf("invalid mode %q", p.Mode)
	}

	ad := &adaptor{id: in.DeviceID, pin: pin, params: p}

	out := registry.BuildOutput{Adaptor: ad}

	// Optional IRQ for inputs where supported.
	if p.Mode == "input" && p.IRQ != nil && ParseEdge(p.IRQ.Edge) != halcore.EdgeNone {
		if irqPin, ok := pin.(halcore.IRQPin); ok {
			out.IRQ = &registry.IRQRequest{
				DevID: in.DeviceID, Pin: irqPin, Edge: ParseEdge(p.IRQ.Edge),
				DebounceMS: p.IRQ.DebounceMS, Invert: p.Invert,
			}
		}
	}
	return out, nil
}

type adaptor struct {
	id     string
	pin    halcore.GPIOPin
	params Params
}

func (a *adaptor) ID() string { return a.id }

func (a *adaptor) Capabilities() []halcore.CapInfo {
	info := map[string]interface{}{
		"pin":            a.pin.Number(),
		"mode":           a.params.Mode,
		"invert":         a.params.Invert,
		"schema_version": 1,
		"driver":         "gpio",
	}
	if a.params.Mode == "input" {
		info["pull"] = a.params.Pull
	}
	return []halcore.CapInfo{{Kind: "gpio", Info: info}}
}

// GPIO is not a periodic producer here; Trigger/Collect are unused.
func (a *adaptor) Trigger(ctx context.Context) (time.Duration, error) {
	return 0, halcore.ErrUnsupported
}
func (a *adaptor) Collect(ctx context.Context) (halcore.Sample, error) {
	return nil, halcore.ErrUnsupported
}

// Control supports basic operations:
//   - inputs:  kind="gpio" method="get" -> {"level":0|1}
//   - outputs: kind="gpio" method="set" payload {"level":bool} (honours inversion); method="toggle"
func (a *adaptor) Control(kind, method string, payload interface{}) (interface{}, error) {
	if kind != "gpio" {
		return nil, halcore.ErrUnsupported
	}
	switch a.params.Mode {
	case "input":
		switch method {
		case "get":
			l := a.pin.Get()
			if a.params.Invert {
				l = !l
			}
			return map[string]interface{}{"level": boolToInt(l)}, nil
		default:
			return nil, halcore.ErrUnsupported
		}
	case "output":
		switch method {
		case "set":
			l, ok := parseLevel(payload)
			if !ok {
				return nil, errors.New("invalid payload; want {\"level\":bool}")
			}
			if a.params.Invert {
				l = !l
			}
			a.pin.Set(l)
			return map[string]interface{}{"ok": true}, nil
		case "toggle":
			a.pin.Toggle()
			return map[string]interface{}{"ok": true}, nil
		default:
			return nil, halcore.ErrUnsupported
		}
	default:
		return nil, halcore.ErrUnsupported
	}
}

func parseLevel(p interface{}) (bool, bool) {
	if m, ok := p.(map[string]interface{}); ok {
		if v, ok := m["level"].(bool); ok {
			return v, true
		}
	}
	return false, false
}

func parsePull(s string) halcore.Pull {
	switch s {
	case "up":
		return halcore.PullUp
	case "down":
		return halcore.PullDown
	default:
		return halcore.PullNone
	}
}

func ParseEdge(s string) halcore.Edge {
	switch s {
	case "rising":
		return halcore.EdgeRising
	case "falling":
		return halcore.EdgeFalling
	case "both":
		return halcore.EdgeBoth
	default:
		return halcore.EdgeNone
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

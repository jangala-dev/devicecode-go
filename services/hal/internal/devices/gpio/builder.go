// services/hal/internal/devices/gpio/builder.go
package gpiodev

import (
	"errors"

	"devicecode-go/services/hal"
)

type builder struct{}

func init() {
	hal.RegisterBuilder("gpio", builder{})
}

func (builder) Build(in hal.BuildInput) (hal.BuildOutput, error) {
	var p hal.GPIOParams
	if err := hal.DecodeJSON(in.ParamsJSON, &p); err != nil {
		return hal.BuildOutput{}, err
	}
	pin, ok := in.Pins.ByNumber(p.Pin)
	if !ok {
		return hal.BuildOutput{}, errors.New("gpio: unknown pin " + string(rune(p.Pin)))
	}

	// Configure initial mode
	if p.Mode == "input" {
		if err := pin.ConfigureInput(parsePullLocal(p.Pull)); err != nil {
			return hal.BuildOutput{}, err
		}
	} else {
		init := false
		if p.Initial != nil {
			init = *p.Initial
		}
		if p.Invert {
			init = !init
		}
		if err := pin.ConfigureOutput(init); err != nil {
			return hal.BuildOutput{}, err
		}
	}

	ad := hal.NewGPIOAdaptor(in.DeviceID, pin, p)
	out := hal.BuildOutput{Adaptor: ad}

	// Optional IRQ
	if p.Mode == "input" && p.IRQ != nil {
		edge := parseEdgeLocal(p.IRQ.Edge)
		if edge != hal.EdgeNone {
			if irqPin, ok := pin.(hal.IRQPin); ok {
				out.IRQ = &hal.IRQRequest{
					DevID:      in.DeviceID,
					Pin:        irqPin,
					Edge:       edge,
					DebounceMS: p.IRQ.DebounceMS,
					Invert:     p.Invert,
				}
			}
		}
	}
	return out, nil
}

// Local conversions to avoid depending on unexported helpers in hal.
func parsePullLocal(s string) hal.Pull {
	switch s {
	case "up", "UP", "pullup":
		return hal.PullUp
	case "down", "DOWN", "pulldown":
		return hal.PullDown
	default:
		return hal.PullNone
	}
}

func parseEdgeLocal(s string) hal.Edge {
	switch s {
	case "rising", "RISE", "rising_edge":
		return hal.EdgeRising
	case "falling", "FALL", "falling_edge":
		return hal.EdgeFalling
	case "both", "BOTH":
		return hal.EdgeBoth
	default:
		return hal.EdgeNone
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }

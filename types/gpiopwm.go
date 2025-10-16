package types

// ------------------------
// Button
// ------------------------

type ButtonInfo struct {
	Pin int `json:"pin"`
}

type ButtonValue struct {
	Pressed bool `json:"pressed"`
}

// ------------------------
// LED (boolean LED; use PWM for brightness)
// ------------------------

type LEDInfo struct {
	Pin int `json:"pin"`
}

type LEDValue struct {
	On bool `json:"on"`
}

type LEDSet struct {
	On bool `json:"on"`
}

// ------------------------
// Switch
// ------------------------

type SwitchInfo struct {
	Pin int `json:"pin"`
}

type SwitchValue struct {
	On bool `json:"on"`
}

type SwitchSet struct {
	On bool `json:"on"`
}

// ------------------------
// PWM
// ------------------------

type PWMInfo struct {
	Pin       int    `json:"pin"`
	Slice     int    `json:"slice,omitempty"`   // provider may fill
	Channel   string `json:"channel,omitempty"` // "A" or "B"
	FreqHz    uint64 `json:"freq_hz,omitempty"`
	Top       uint16 `json:"top,omitempty"`
	ActiveLow bool   `json:"active_low"`
	Initial   uint16 `json:"initial"`
}

type PWMValue struct {
	Level uint16 `json:"level"` // 0..Top (logical level)
}

type PWMSet struct {
	Level uint16 `json:"level"` // 0..Top (logical)
}

// PWMRampMode mirrors the HAL/provider modes.
type PWMRampMode uint8

const (
	PWMRampLinear PWMRampMode = iota // evenly spaced absolute steps
	// future: gamma/exponential/trapezoid...
)

type PWMRamp struct {
	To         uint16      `json:"to"`          // 0..Top (logical)
	DurationMs uint32      `json:"duration_ms"` // total duration
	Steps      uint16      `json:"steps"`       // >0
	Mode       PWMRampMode `json:"mode"`        // 0=linear
}

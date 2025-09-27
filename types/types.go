package types

// ---- Common HAL state (retained) ----

type HALState struct {
	Level  string `json:"level"`  // e.g. "idle", "ready", "stopped"
	Status string `json:"status"` // freeform short code
	TSms   int64  `json:"ts_ms"`
}

const (
	LinkUp       = "up"
	LinkDown     = "down"
	LinkDegraded = "degraded"
)

type CapabilityStatus struct {
	Link  string `json:"link"`
	TSms  int64  `json:"ts_ms"`
	Error string `json:"error,omitempty"`
}

// ---- Capability kinds & info ----

type Kind string

const (
	KindLED Kind = "led"
)

// Info envelope each device/cap exposes (retained)
type Info struct {
	SchemaVersion int         `json:"schema_version"`
	Driver        string      `json:"driver"`
	Detail        interface{} `json:"detail,omitempty"`
}

// ---- LED capability params ----

type LEDParams struct {
	Pin     int  `json:"pin"`
	Initial bool `json:"initial,omitempty"`
}

// ---- LED capability payloads ----

type LEDInfo struct {
	Pin int `json:"pin"`
}

type LEDValue struct {
	Level uint8 `json:"level"` // 0 or 1
}

// Controls
type LEDSet struct {
	Level bool `json:"level"`
}

const (
	KindSwitch Kind = "switch"
)

type SwitchValue struct{ On bool }
type SwitchSet struct{ On bool }  // control payload
type SwitchInfo struct{ Pin int } // mirror LEDInfo shape if you like

const (
	KindPWM Kind = "pwm"
)

// PWMInfo is published under hal/cap/.../info as Info.Detail.
type PWMInfo struct {
	Pin     int    `json:"pin"`
	Slice   int    `json:"slice,omitempty"`   // optional: provider may fill if known
	Channel string `json:"channel,omitempty"` // "A" or "B", optional
	FreqHz  uint32 `json:"freqHz,omitempty"`  // optional: device/provider may fill
	Top     uint16 `json:"top,omitempty"`     // optional: device/provider may fill
}

// PWMValue is published under hal/cap/.../value (retained).
type PWMValue struct {
	Level uint16 `json:"level"` // 0..Top
}

// Control payloads
type PWMSet struct {
	Level uint16 `json:"level"` // 0..Top
}

type PWMRamp struct {
	To         uint16 `json:"to"`         // 0..Top
	DurationMs uint32 `json:"durationMs"` // total duration
	Steps      uint16 `json:"steps"`      // number of steps (>0)
	Mode       uint8  `json:"mode"`       // 0 = linear (maps to core.PWMRampLinear)
}

// Generic replies
type OKReply struct {
	OK bool `json:"ok"`
}
type ErrorReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// ---- Public HAL configuration ----

type HALConfig struct {
	Devices []HALDevice `json:"devices"`
}

type HALDevice struct {
	ID     string      `json:"id"`     // logical device id, e.g. "led0"
	Type   string      `json:"type"`   // e.g. "gpio_led"
	Params interface{} `json:"params"` // device-specific params (JSON-like)
}

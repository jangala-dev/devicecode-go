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

type CapabilityState struct {
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

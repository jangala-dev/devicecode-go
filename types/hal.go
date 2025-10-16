package types

// ------------------------
// Common HAL state (retained)
// ------------------------

type HALState struct {
	Level  string `json:"level"`  // "idle", "ready", "stopped"
	Status string `json:"status"` // freeform short code
	TS     int64  `json:"ts_ns"`  // publish Unix ns (matches HAL)
}

// Link is the link/state reported for a capability.
type Link string

const (
	LinkUp       Link = "up"
	LinkDown     Link = "down"
	LinkDegraded Link = "degraded"
)

type CapabilityStatus struct {
	Link  Link   `json:"link"`
	TS    int64  `json:"ts_ns"`           // Unix ns (matches HAL)
	Error string `json:"error,omitempty"` // machine-readable short code
}

// ------------------------
// Polling (control + declarative)
// ------------------------

type PollStart struct {
	Verb       string `json:"verb"`        // e.g. "read"
	IntervalMs uint32 `json:"interval_ms"` // >0
	JitterMs   uint16 `json:"jitter_ms"`   // uniform [0..JitterMs]
}

type PollStop struct {
	Verb string `json:"verb,omitempty"` // empty => "read"
}

type PollSpec struct {
	Domain     string `json:"domain"`      // e.g. "env"
	Kind       Kind   `json:"kind"`        // e.g. "temperature"
	Name       string `json:"name"`        // e.g. "core"
	Verb       string `json:"verb"`        // typically "read"
	IntervalMs uint32 `json:"interval_ms"` // >0
	JitterMs   uint16 `json:"jitter_ms"`   // optional
}

// ------------------------
// HAL configuration
// ------------------------

type HALConfig struct {
	Devices []HALDevice `json:"devices"`
	Pollers []PollSpec  `json:"pollers,omitempty"`
}

type HALDevice struct {
	ID     string      `json:"id"`     // logical device id
	Type   string      `json:"type"`   // e.g. "gpio_led"
	Params interface{} `json:"params"` // device-specific params (JSON-like)
}

// ------------------------
// Generic replies
// ------------------------

type OKReply struct {
	OK bool `json:"ok"`
}

type ErrorReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// ------------------------
// Info envelope (retained)
// ------------------------

type Info struct {
	SchemaVersion int         `json:"schema_version"`
	Driver        string      `json:"driver"`
	Detail        interface{} `json:"detail,omitempty"` // one of *Info types below
}

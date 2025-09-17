// services/hal/internal/consts/consts.go
package consts

// Top-level topics
const (
	TokConfig     = "config"
	TokHAL        = "hal"
	TokCapability = "capability"
	TokInfo       = "info"
	TokState      = "state"
	TokValue      = "value"
	TokControl    = "control"
	TokEvent      = "event"
)

// Control verbs
const (
	CtrlReadNow = "read_now"
	CtrlSetRate = "set_rate"
)

// Capability kinds used in service wiring
const (
	KindGPIO = "gpio"
	KindUART = "uart"
)

const (
	LinkUp       = "up"
	LinkDown     = "down"
	LinkDegraded = "degraded"
)

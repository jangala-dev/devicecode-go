package types

// ---- Common HAL state (retained) ----

type HALState struct {
	Level  string `json:"level"`  // e.g. "idle", "ready", "stopped"
	Status string `json:"status"` // freeform short code
	TS     int64  `json:"ts_ms"`
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
	TS    int64  `json:"ts_ms"`
	Error string `json:"error,omitempty"`
}

// ---- Capability kinds & info ----

type Kind string

const (
	KindLED         Kind = "led"
	KindSwitch      Kind = "switch"
	KindPWM         Kind = "pwm"
	KindTemperature Kind = "temperature"
	KindHumidity    Kind = "humidity"
	KindSerial      Kind = "serial"
	KindButton      Kind = "button"
	KindBattery     Kind = "battery"
	KindCharger     Kind = "charger"
)

// Info envelope each device/cap exposes (retained)
type Info struct {
	SchemaVersion int         `json:"schema_version"`
	Driver        string      `json:"driver"`
	Detail        interface{} `json:"detail,omitempty"`
}

type ButtonInfo struct{ Pin int }

type ButtonValue struct{ Pressed bool }

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

type SwitchValue struct{ On bool }
type SwitchSet struct{ On bool } // control payload
type SwitchInfo struct{ Pin int }

const ()

// PWMInfo is published under hal/cap/.../info as Info.Detail.
type PWMInfo struct {
	Pin       int    `json:"pin"`
	Slice     int    `json:"slice,omitempty"`   // optional: provider may fill if known
	Channel   string `json:"channel,omitempty"` // "A" or "B", optional
	FreqHz    uint64 `json:"freqHz,omitempty"`  // optional: device/provider may fill
	Top       uint16 `json:"top,omitempty"`     // optional: device/provider may fill
	ActiveLow bool
	Initial   uint16
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

// ---- Serial capability (control + discovery) ----

// SerialParity is a small enum to avoid string parsing on device side.
type Parity uint8

const (
	ParityNone Parity = iota
	ParityEven
	ParityOdd
)

// Control payloads
// Emitted on session_open success as a *tagged* event payload.
// Keep handles as plain uint32 so the schema is decoupled from shmring internals.
type SerialSessionOpen struct {
	// Power-of-two sizes (bytes). Device will default if zero.
	RXSize int `json:"rx_size,omitempty"`
	TXSize int `json:"tx_size,omitempty"`
}

type SerialSessionClose struct{}

type SerialSetBaud struct {
	Baud uint32
}

type SerialSetFormat struct {
	DataBits uint8  `json:"data_bits"`
	StopBits uint8  `json:"stop_bits"`
	Parity   Parity `json:"parity"`
}

type SerialSessionOpened struct {
	SessionID uint32 `json:"session_id"`
	RXHandle  uint32 `json:"rx_handle"`
	TXHandle  uint32 `json:"tx_handle"`
}

// Discovery payload for Info.Detail
type SerialInfo struct {
	Bus  string
	Baud uint32 // 0 if unspecified
}

// ---- Public HAL configuration ----

type HALConfig struct {
	Devices []HALDevice `json:"devices"`
	Pollers []PollSpec
}

type HALDevice struct {
	ID     string `json:"id"`     // logical device id, e.g. "led0"
	Type   string `json:"type"`   // e.g. "gpio_led"
	Params any    `json:"params"` // device-specific params (JSON-like)
}

// Info structs appear on hal/capability/<kind>/<id>/info (retained).
type TemperatureInfo struct {
	Sensor string // e.g. "aht20", "shtc3"
	Addr   uint16 // I2C address
	Bus    string // e.g. "i2c0"
}

type HumidityInfo struct {
	Sensor string
	Addr   uint16
	Bus    string
}

// Value payloads appear on hal/capability/<kind>/<id>/value (retained).
// Fixed-point, small types to suit TinyGo.

type TemperatureValue struct {
	// Tenths of °C (e.g. 231 => 23.1°C). Range comfortably fits int16.
	DeciC int16
}

type HumidityValue struct {
	// Hundredths of %RH (e.g. 5034 => 50.34 %RH). Use uint16 (0..10000).
	RHx100 uint16
}

// PollStart is the strictly-typed payload for starting/updating a schedule.
type PollStart struct {
	Verb       string // required, e.g. "read"
	IntervalMs uint32 // required, >0
	JitterMs   uint16 // optional, uniform [0..JitterMs]
}

// PollStop is the strictly-typed payload for stopping a schedule.
// If Verb is empty, it is treated as "read".
type PollStop struct {
	Verb string
}

// PollSpec is a declarative, config-time schedule attached to HALConfig.
// HAL applies these at startup (and whenever a new config is applied).
type PollSpec struct {
	Domain     string // capability domain, e.g. "env"
	Kind       string // capability kind,   e.g. "temperature"
	Name       string // capability name,   e.g. "core"
	Verb       string // control verb to call, typically "read"
	IntervalMs uint32 // >0
	JitterMs   uint16 // optional
}

type BatteryInfo struct {
	Cells      uint8
	Chem       string // "li" | "leadacid" | "auto"
	RSNSB_uOhm uint32
	Bus        string
	Addr       uint16
}

// Retained value published at hal/cap/power/battery/<name>/value
type BatteryValue struct {
	PackMilliV      int32  // total pack voltage (mV)
	PerCellMilliV   int32  // per-cell voltage (mV)
	IBatMilliA      int32  // battery current (mA; sign by device convention)
	TempMilliC      int32  // die temperature (milli-°C)
	BSR_uOhmPerCell uint32 // battery sense resistance estimate per cell
}

type ChargerInfo struct {
	RSNSI_uOhm uint32
	Bus        string
	Addr       uint16
}

// Retained value published at hal/cap/power/charger/<name>/value
type ChargerValue struct {
	VIN_mV  int32
	VSYS_mV int32
	IIn_mA  int32
	State   uint16 // raw CHARGER_STATE bits
	Status  uint16 // raw CHARGE_STATUS bits
	Sys     uint16 // raw SYSTEM_STATUS bits
}

// Controls
type ChargerEnable struct{ On bool }           // verb: "enable"
type SetInputLimit struct{ MilliA int32 }      // verb: "set_input_limit"
type SetChargeTarget struct{ MilliA int32 }    // verb: "set_charge_target"
type SetVinWindow struct{ Lo_mV, Hi_mV int32 } // verb: "set_vin_window"

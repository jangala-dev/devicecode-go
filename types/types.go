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
// Capability addressing & kinds
// ------------------------

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

// CapabilityAddress identifies a public capability on the bus.
type CapabilityAddress struct {
	Domain string `json:"domain"` // e.g. "io","power","env"
	Kind   Kind   `json:"kind"`
	Name   string `json:"name"`
}

// ------------------------
// Info envelope (retained)
// ------------------------

type Info struct {
	SchemaVersion int         `json:"schema_version"`
	Driver        string      `json:"driver"`
	Detail        interface{} `json:"detail,omitempty"` // one of *Info types below
}

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

// ------------------------
// Serial
// ------------------------

type Parity uint8

const (
	ParityNone Parity = iota
	ParityEven
	ParityOdd
)

type SerialSessionOpen struct {
	// Power-of-two sizes (bytes). Device will default if zero.
	RXSize int `json:"rx_size,omitempty"`
	TXSize int `json:"tx_size,omitempty"`
}

type SerialSessionClose struct{}

type SerialSetBaud struct {
	Baud uint32 `json:"baud"`
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

type SerialInfo struct {
	Bus  string `json:"bus"`
	Baud uint32 `json:"baud"` // 0 if unspecified
}

// ------------------------
// Temperature & humidity
// ------------------------

type TemperatureInfo struct {
	Sensor string `json:"sensor"` // "aht20", "shtc3", ...
	Addr   uint16 `json:"addr"`   // I2C address
	Bus    string `json:"bus"`    // "i2c0", ...
}

type HumidityInfo struct {
	Sensor string `json:"sensor"`
	Addr   uint16 `json:"addr"`
	Bus    string `json:"bus"`
}

type TemperatureValue struct {
	// Tenths of °C (e.g. 231 => 23.1°C).
	DeciC int16 `json:"deci_c"`
}

type HumidityValue struct {
	// Hundredths of %RH (0..10000 for 0..100.00%).
	RHx100 uint16 `json:"rh_x100"`
}

// ------------------------
// Battery / Charger (ltc4015)
// ------------------------

type BatteryInfo struct {
	Cells      uint8  `json:"cells"`
	Chem       string `json:"chem"`       // "li" | "leadacid" | "auto"
	RSNSB_uOhm uint32 `json:"rsnsb_uohm"` // battery sense
	Bus        string `json:"bus"`
	Addr       uint16 `json:"addr"`
}

// Retained value: hal/cap/power/battery/<name>/value
type BatteryValue struct {
	PackMilliV      int32  `json:"pack_mV"`
	PerCellMilliV   int32  `json:"per_cell_mV"`
	IBatMilliA      int32  `json:"ibat_mA"`
	TempMilliC      int32  `json:"temp_mC"`
	BSR_uOhmPerCell uint32 `json:"bsr_uohm_per_cell"`
}

type ChargerInfo struct {
	RSNSI_uOhm uint32 `json:"rsnsi_uohm"`
	Bus        string `json:"bus"`
	Addr       uint16 `json:"addr"`
}

// Retained value: hal/cap/power/charger/<name>/value
type ChargerValue struct {
	VIN_mV  int32  `json:"vin_mV"`
	VSYS_mV int32  `json:"vsys_mV"`
	IIn_mA  int32  `json:"iin_mA"`
	State   uint16 `json:"state"`  // raw CHARGER_STATE bits
	Status  uint16 `json:"status"` // raw CHARGE_STATUS bits
	Sys     uint16 `json:"sys"`    // raw SYSTEM_STATUS bits
}

// Controls
type ChargerEnable struct{ On bool }           // verb: "enable"
type SetInputLimit struct{ MilliA int32 }      // verb: "set_input_limit"
type SetChargeTarget struct{ MilliA int32 }    // verb: "set_charge_target"
type SetVinWindow struct{ Lo_mV, Hi_mV int32 } // verb: "set_vin_window"

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

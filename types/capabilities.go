package types

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

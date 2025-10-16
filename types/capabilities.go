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

func (k Kind) Valid() bool {
	switch k {
	case KindLED, KindSwitch, KindPWM, KindTemperature, KindHumidity,
		KindSerial, KindButton, KindBattery, KindCharger:
		return true
	}
	return false
}
func (k Kind) String() string { return string(k) }

// CapabilityAddress identifies a public capability on the bus.
type CapabilityAddress struct {
	Domain string `json:"domain"` // e.g. "io","power","env"
	Kind   Kind   `json:"kind"`
	Name   string `json:"name"`
}

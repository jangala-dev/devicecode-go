package types

// New capability kinds
const (
	KindTemperature Kind = "temp"
	KindHumidity    Kind = "humid"
)

// Info structs appear on hal/capability/<kind>/<id>/info (retained).
type TemperatureInfo struct {
	Sensor string // e.g. "aht20", "shtc3"
	Addr   uint16 // I²C address
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

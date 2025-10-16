package types

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

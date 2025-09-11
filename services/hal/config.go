package hal

// Minimal JSON config structures.

type HALConfig struct {
	Version int      `json:"version"`
	Buses   []BusCfg `json:"buses"`
	Devices []DevCfg `json:"devices"`
}

type BusCfg struct {
	ID     string   `json:"id"`   // "i2c0"
	Type   string   `json:"type"` // "i2c"
	Impl   string   `json:"impl"` // e.g. "tinygo" (informational)
	Pins   []PinCfg `json:"pins"` // wiring is applied by the platform factory
	Params struct {
		FreqHz int `json:"freq_hz"`
	} `json:"params"`
}

type PinCfg struct {
	Name   string `json:"name"`
	Signal string `json:"signal"`
}

type DevCfg struct {
	ID     string    `json:"id"`   // "aht20-0"
	Type   string    `json:"type"` // "aht20"
	BusRef DevBusRef `json:"bus_ref"`
	Params any       `json:"params,omitempty"` // device-specific shape; may be a map or struct
}

type DevBusRef struct{ ID, Type string }

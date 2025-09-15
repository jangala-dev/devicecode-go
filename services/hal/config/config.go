package config

// HALConfig is supplied on the "config/hal" bus topic.
type HALConfig struct {
	Devices []Device `json:"devices"`
}

// Device describes one physical or logical device to be managed by HAL.
type Device struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Params any    `json:"params,omitempty"`
	BusRef BusRef `json:"bus_ref,omitempty"` // for shared-bus devices (e.g. I²C)
}

// BusRef identifies a named bus instance previously configured in the platform layer.
type BusRef struct {
	Type string `json:"type"` // e.g. "i2c", "spi"
	ID   string `json:"id"`   // e.g. "i2c0"
}

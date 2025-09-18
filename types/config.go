package types

// HAL configuration supplied on topic "config/hal".

type HALConfig struct {
	Devices []Device `json:"devices"`
}

type Device struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Params any    `json:"params,omitempty"`
	BusRef BusRef `json:"bus_ref,omitempty"`
}

type BusRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

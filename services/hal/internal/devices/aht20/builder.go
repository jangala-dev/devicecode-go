package aht20dev

import (
	"errors"
	"time"

	"devicecode-go/services/hal"
)

type builder struct{}

func init() {
	hal.RegisterBuilder("aht20", builder{})
}

func (builder) Build(in hal.BuildInput) (hal.BuildOutput, error) {
	if in.BusRef.Type != "i2c" || in.BusRef.ID == "" {
		return hal.BuildOutput{}, errors.New("aht20: missing or invalid i2c bus reference")
	}
	i2c, ok := in.Buses.ByID(in.BusRef.ID)
	if !ok {
		return hal.BuildOutput{}, errors.New("aht20: unknown i2c bus " + in.BusRef.ID)
	}
	var p struct {
		Addr int `json:"addr"`
	}
	_ = hal.DecodeJSON(in.ParamsJSON, &p)
	if p.Addr == 0 {
		p.Addr = 0x38
	}
	ad := hal.NewAHT20Adaptor(in.DeviceID, i2c, uint16(p.Addr))
	return hal.BuildOutput{
		Adaptor:     ad,
		BusID:       in.BusRef.ID,
		SampleEvery: 2 * time.Second,
	}, nil
}

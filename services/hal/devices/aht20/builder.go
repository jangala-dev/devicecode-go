package aht20dev

import (
	"context"
	"errors"
	"time"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/drvshim"
	"devicecode-go/types"

	"devicecode-go/drivers/aht20"
)

func init() { core.RegisterBuilder("aht20", builder{}) }

type Params struct {
	Bus  string // e.g. "i2c0"
	Addr uint16 // 0x38 default
}

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if p.Addr == 0 {
		p.Addr = 0x38
	}
	if !ok || p.Bus == "" || p.Addr == 0 {
		return nil, errors.New("invalid_params")
	}
	own, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}
	return &Device{
		id:   in.ID,
		bus:  p.Bus,
		addr: p.Addr,
		i2c:  own,
		pub:  in.Res.Pub,
	}, nil
}

type Device struct {
	id   string
	bus  string
	addr uint16

	i2c core.I2COwner
	pub core.EventEmitter

	capTemp core.CapID
	capHum  core.CapID
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{
		{
			Domain: "env",
			Kind:   types.KindTemperature,
			Name:   d.id,
			Info: types.Info{
				SchemaVersion: 1,
				Driver:        "aht20",
				Detail:        types.TemperatureInfo{Sensor: "aht20", Addr: d.addr, Bus: d.bus},
			},
		},
		{
			Domain: "env",
			Kind:   types.KindHumidity,
			Name:   d.id,
			Info: types.Info{
				SchemaVersion: 1,
				Driver:        "aht20",
				Detail:        types.HumidityInfo{Sensor: "aht20", Addr: d.addr, Bus: d.bus},
			},
		},
	}
}

func (d *Device) BindCapabilities(ids []core.CapID) {
	// Expect exactly 2: [temperature, humidity] in Capabilities() order.
	if len(ids) >= 1 {
		d.capTemp = ids[0]
	}
	if len(ids) >= 2 {
		d.capHum = ids[1]
	}
}

func (d *Device) Init(ctx context.Context) error {
	// No goroutine. Device is passive; reads happen when commanded.
	return nil
}

func (d *Device) Close() error { return nil }

func (d *Device) Control(_ core.CapID, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "read":
		ok := d.i2c.TryEnqueue(func(bus core.I2CBus) error {
			// Construct a driver bound to the worker's bus.
			b := drvshim.NewI2CFromBus(bus).WithTimeout(50)
			drv := aht20.New(b)

			start := time.Now().UnixMilli()
			if err := drv.Read(); err != nil {
				d.emitErr(err.Error(), start)
				return nil
			}
			decic := drv.DeciCelsius()
			if decic > 32767 {
				decic = 32767
			}
			if decic < -32768 {
				decic = -32768
			}
			rhx100 := drv.DeciRelHumidity() * 10
			if rhx100 < 0 {
				rhx100 = 0
			}
			if rhx100 > 10000 {
				rhx100 = 10000
			}
			ts := time.Now().UnixMilli()
			d.pub.Emit(core.Event{
				CapID:   d.capTemp,
				Payload: types.TemperatureValue{DeciC: int16(decic)},
				TSms:    ts,
			})
			d.pub.Emit(core.Event{
				CapID:   d.capHum,
				Payload: types.HumidityValue{RHx100: uint16(rhx100)},
				TSms:    ts,
			})
			return nil
		})
		if !ok {
			return core.EnqueueResult{OK: false, Error: "busy"}, nil
		}
		return core.EnqueueResult{OK: true}, nil
	default:
		return core.EnqueueResult{OK: false, Error: "unsupported"}, nil
	}
}

func (d *Device) emitErr(code string, t0 int64) {
	d.pub.Emit(core.Event{
		CapID: d.capTemp,
		Err:   code,
		TSms:  t0,
	})
	d.pub.Emit(core.Event{
		CapID: d.capHum,
		Err:   code,
		TSms:  t0,
	})
}

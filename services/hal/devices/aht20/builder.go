package aht20dev

import (
	"context"
	"time"

	"devicecode-go/errcode"
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
		return nil, errcode.InvalidParams
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

	addrTemp core.CapAddr
	addrHum  core.CapAddr
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{
		{
			Domain: "env",
			Kind:   types.KindTemperature,
			Name:   d.id,
			Info: types.Info{
				SchemaVersion: 1, Driver: "aht20",
				Detail: types.TemperatureInfo{Sensor: "aht20", Addr: d.addr, Bus: d.bus},
			},
		},
		{
			Domain: "env",
			Kind:   types.KindHumidity,
			Name:   d.id,
			Info: types.Info{
				SchemaVersion: 1, Driver: "aht20",
				Detail: types.HumidityInfo{Sensor: "aht20", Addr: d.addr, Bus: d.bus},
			},
		},
	}
}

func (d *Device) Init(ctx context.Context) error {
	d.addrTemp = core.CapAddr{Domain: "env", Kind: string(types.KindTemperature), Name: d.id}
	d.addrHum = core.CapAddr{Domain: "env", Kind: string(types.KindHumidity), Name: d.id}
	return nil
}

func (d *Device) Close() error { return nil }

func (d *Device) Control(_ core.CapAddr, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "read":
		ok := d.i2c.TryEnqueue(func(bus core.I2CBus) error {
			b := drvshim.NewI2CFromBus(bus).WithTimeout(50)
			drv := aht20.New(b)

			start := time.Now().UnixMilli()
			if err := drv.Read(); err != nil {
				d.emitErr(string(errcode.MapDriverErr(err)), start)
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
				Addr:    d.addrTemp,
				Payload: types.TemperatureValue{DeciC: int16(decic)},
				TSms:    ts,
			})
			d.pub.Emit(core.Event{
				Addr:    d.addrHum,
				Payload: types.HumidityValue{RHx100: uint16(rhx100)},
				TSms:    ts,
			})
			return nil
		})
		if !ok {
			return core.EnqueueResult{OK: false, Error: errcode.Busy}, nil
		}
		return core.EnqueueResult{OK: true}, nil
	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

func (d *Device) emitErr(code string, t0 int64) {
	d.pub.Emit(core.Event{Addr: d.addrTemp, Err: code, TSms: t0})
	d.pub.Emit(core.Event{Addr: d.addrHum, Err: code, TSms: t0})
}

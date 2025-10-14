// services/hal/devices/aht20/builder.go
package aht20dev

import (
	"context"
	"sync/atomic"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"

	"devicecode-go/drivers/aht20"

	"tinygo.org/x/drivers"
)

func init() { core.RegisterBuilder("aht20", builder{}) }

type Params struct {
	Bus    string // e.g. "i2c0"
	Addr   uint16 // defaults to aht20.Address (0x38) if zero
	Domain string // REQUIRED
	Name   string // REQUIRED
}

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok || p.Bus == "" {
		return nil, errcode.InvalidParams
	}
	if p.Domain == "" || p.Name == "" {
		return nil, errcode.InvalidParams
	}
	if p.Addr == 0 {
		p.Addr = aht20.Address
	}
	bus, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}

	d := &Device{
		id:   in.ID,
		bus:  p.Bus,
		addr: p.Addr,
		i2c:  bus,
		pub:  in.Res.Pub,
		reg:  in.Res.Reg,
		dom:  p.Domain,
		name: p.Name,
	}
	d.drv = aht20.New(bus) // drivers.I2C directly
	return d, nil
}

type Device struct {
	id   string
	bus  string
	addr uint16

	i2c drivers.I2C
	pub core.EventEmitter
	reg core.ResourceRegistry

	drv  aht20.Device
	dom  string
	name string

	addrTemp core.CapAddr
	addrHum  core.CapAddr

	reading atomic.Uint32
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{
		{
			Domain: d.dom,
			Kind:   types.KindTemperature,
			Name:   d.name,
			Info: types.Info{
				SchemaVersion: 1, Driver: "aht20",
				Detail: types.TemperatureInfo{Sensor: "aht20", Addr: d.addr, Bus: d.bus},
			},
		},
		{
			Domain: d.dom,
			Kind:   types.KindHumidity,
			Name:   d.name,
			Info: types.Info{
				SchemaVersion: 1, Driver: "aht20",
				Detail: types.HumidityInfo{Sensor: "aht20", Addr: d.addr, Bus: d.bus},
			},
		},
	}
}

func (d *Device) Init(ctx context.Context) error {
	// Establish capability addresses; avoid touching the bus here.
	d.addrTemp = core.CapAddr{Domain: d.dom, Kind: types.KindTemperature, Name: d.name}
	d.addrHum = core.CapAddr{Domain: d.dom, Kind: types.KindHumidity, Name: d.name}
	// Provide the address without doing I²C; Configure() will occur on first Read.
	d.drv.Address = d.addr
	return nil
}

func (d *Device) Close() error {
	if d.reg != nil {
		d.reg.ReleaseI2C(d.id, core.ResourceID(d.bus))
	}
	return nil
}

func (d *Device) Control(_ core.CapAddr, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "read":
		if d.reading.Swap(1) == 1 {
			return core.EnqueueResult{OK: false, Error: errcode.Busy}, nil
		}
		go func() {
			defer d.reading.Store(0)
			d.readOnce()
		}()
		return core.EnqueueResult{OK: true}, nil
	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

func (d *Device) readOnce() {
	// Configure (idempotent) and read.
	d.drv.Configure(aht20.Config{
		Address:        d.addr,
		PollInterval:   15 * time.Millisecond,
		CollectTimeout: 250 * time.Millisecond,
		TriggerHint:    80 * time.Millisecond,
	})

	if err := d.drv.Read(); err != nil {
		d.emitErr(string(errcode.MapDriverErr(err)))
		return
	}

	// Fixed-point conversions with sensor-specific bounds (AHT20: −40..125 °C; 0..100 %RH)
	decic := int32(d.drv.DeciCelsius())           // deci-°C
	rhx100 := int32(d.drv.DeciRelHumidity() * 10) // %RH ×100

	const (
		tMin = -375 // −37.5 °C
		tMax = 825  // 82.5 °C
		hMin = 0
		hMax = 10000
	)

	// Hard-range validation: if outside, treat as a failed sample.
	if decic < tMin || decic > tMax || rhx100 < hMin || rhx100 > hMax {
		d.emitErr("invalid_sample")
		return
	}

	// Publish retained values
	d.pub.Emit(core.Event{
		Addr:    d.addrTemp,
		Payload: types.TemperatureValue{DeciC: int16(decic)},
	})
	d.pub.Emit(core.Event{
		Addr:    d.addrHum,
		Payload: types.HumidityValue{RHx100: uint16(rhx100)},
	})
}

func (d *Device) emitErr(code string) {
	d.pub.Emit(core.Event{Addr: d.addrTemp, Err: code})
	d.pub.Emit(core.Event{Addr: d.addrHum, Err: code})
}

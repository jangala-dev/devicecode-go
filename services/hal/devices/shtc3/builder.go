package shtc3dev

import (
	"context"
	"sync/atomic"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"

	"tinygo.org/x/drivers"
	"tinygo.org/x/drivers/shtc3"
)

func init() { core.RegisterBuilder("shtc3", builder{}) }

type Params struct {
	Bus    string // e.g. "i2c0"
	Domain string
	Name   string
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
	bus, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}

	d := &Device{
		id:   in.ID,
		bus:  p.Bus,
		i2c:  bus,
		pub:  in.Res.Pub,
		reg:  in.Res.Reg,
		dom:  p.Domain,
		name: p.Name,
	}
	d.drv = shtc3.New(bus) // drivers.I2C directly
	return d, nil
}

type Device struct {
	id  string
	bus string

	i2c drivers.I2C
	pub core.EventEmitter
	reg core.ResourceRegistry

	drv  shtc3.Device
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
				SchemaVersion: 1, Driver: "shtc3",
				Detail: types.TemperatureInfo{Sensor: "shtc3", Addr: 0x70, Bus: d.bus},
			},
		},
		{
			Domain: d.dom,
			Kind:   types.KindHumidity,
			Name:   d.name,
			Info: types.Info{
				SchemaVersion: 1, Driver: "shtc3",
				Detail: types.HumidityInfo{Sensor: "shtc3", Addr: 0x70, Bus: d.bus},
			},
		},
	}
}

// Init sets up addresses without touching the bus.
func (d *Device) Init(ctx context.Context) error {
	d.addrTemp = core.CapAddr{Domain: d.dom, Kind: types.KindTemperature, Name: d.name}
	d.addrHum = core.CapAddr{Domain: d.dom, Kind: types.KindHumidity, Name: d.name}
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
	// Wake, read, sleep in the same goroutine; driver is blocking.
	_ = d.drv.WakeUp()
	defer func() { _ = d.drv.Sleep() }()

	t0 := time.Now().UnixNano()
	tmc, rhx100, err := d.drv.ReadTemperatureHumidity()
	if err != nil {
		d.emitErr(string(errcode.MapDriverErr(err)), t0)
		return
	}

	// Convert to deci-°C and %RH×100
	decic := int32(tmc / 100) // milli-°C → deci-°C
	rh := int32(rhx100)       // already ×100

	const (
		tMin = -375 // −37.5 °C
		tMax = 1175 // 117.5 °C
		hMin = 0
		hMax = 10000
	)

	if decic < tMin || decic > tMax || rh < hMin || rh > hMax {
		d.emitErr("invalid_sample", t0)
		return
	}

	d.pub.Emit(core.Event{
		Addr:    d.addrTemp,
		Payload: types.TemperatureValue{DeciC: int16(decic)},
	})
	d.pub.Emit(core.Event{
		Addr:    d.addrHum,
		Payload: types.HumidityValue{RHx100: uint16(rh)},
	})
}

func (d *Device) emitErr(code string, t0 int64) {
	d.pub.Emit(core.Event{Addr: d.addrTemp, Err: code})
	d.pub.Emit(core.Event{Addr: d.addrHum, Err: code})
}

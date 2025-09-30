package shtc3dev

import (
	"context"
	"sync/atomic"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
	"devicecode-go/x/mathx"

	"tinygo.org/x/drivers"
	"tinygo.org/x/drivers/shtc3"
)

func init() { core.RegisterBuilder("shtc3", builder{}) }

type Params struct {
	Bus string // e.g. "i2c0"
}

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok || p.Bus == "" {
		return nil, errcode.InvalidParams
	}
	bus, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}

	d := &Device{
		id:  in.ID,
		bus: p.Bus,
		i2c: bus,
		pub: in.Res.Pub,
		reg: in.Res.Reg,
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

	drv      shtc3.Device
	addrTemp core.CapAddr
	addrHum  core.CapAddr

	reading atomic.Uint32
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{
		{
			Domain: "env",
			Kind:   types.KindTemperature,
			Name:   d.id,
			Info: types.Info{
				SchemaVersion: 1, Driver: "shtc3",
				Detail: types.TemperatureInfo{Sensor: "shtc3", Addr: 0x70, Bus: d.bus},
			},
		},
		{
			Domain: "env",
			Kind:   types.KindHumidity,
			Name:   d.id,
			Info: types.Info{
				SchemaVersion: 1, Driver: "shtc3",
				Detail: types.HumidityInfo{Sensor: "shtc3", Addr: 0x70, Bus: d.bus},
			},
		},
	}
}

// Init sets up addresses without touching the bus.
func (d *Device) Init(ctx context.Context) error {
	d.addrTemp = core.CapAddr{Domain: "env", Kind: string(types.KindTemperature), Name: d.id}
	d.addrHum = core.CapAddr{Domain: "env", Kind: string(types.KindHumidity), Name: d.id}
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

	// Convert: tmc is milli-°C; publish deci-°C. Clamp ranges.
	decic := mathx.Clamp(tmc/100, -32768, 32767)
	rhx100 = mathx.Clamp(rhx100, 0, 10000)

	ts := time.Now().UnixNano()
	d.pub.Emit(core.Event{
		Addr:    d.addrTemp,
		Payload: types.TemperatureValue{DeciC: int16(decic)},
		TS:      ts,
	})
	d.pub.Emit(core.Event{
		Addr:    d.addrHum,
		Payload: types.HumidityValue{RHx100: uint16(rhx100)},
		TS:      ts,
	})
}

func (d *Device) emitErr(code string, t0 int64) {
	d.pub.Emit(core.Event{Addr: d.addrTemp, Err: code, TS: t0})
	d.pub.Emit(core.Event{Addr: d.addrHum, Err: code, TS: t0})
}

// services/hal/devices/aht20/builder.go
package aht20dev

import (
	"context"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/drvshim"
	"devicecode-go/types"
	"devicecode-go/x/mathx"

	"devicecode-go/drivers/aht20"
)

func init() { core.RegisterBuilder("aht20", builder{}) }

type Params struct {
	Bus  string // e.g. "i2c0"
	Addr uint16 // defaults to aht20.Address (0x38) if zero
}

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok || p.Bus == "" {
		return nil, errcode.InvalidParams
	}
	if p.Addr == 0 {
		p.Addr = aht20.Address
	}
	own, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}

	d := &Device{
		id:   in.ID,
		bus:  p.Bus,
		addr: p.Addr,
		i2c:  own,
		pub:  in.Res.Pub,
		reg:  in.Res.Reg,
	}

	// Persistent hot-swappable I2C shim and persistent driver instance.
	d.hot = &drvshim.HotI2C{}
	d.drv = aht20.New(d.hot)

	// Reusable, closure-free job object.
	d.jobRead = &aht20ReadJob{d: d}

	return d, nil
}

type Device struct {
	id   string
	bus  string
	addr uint16

	i2c core.I2COwner
	pub core.EventEmitter
	reg core.ResourceRegistry

	// Persisted bus shim and driver (no per-call allocations).
	hot     *drvshim.HotI2C
	drv     aht20.Device
	jobRead *aht20ReadJob

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
	// Establish capability addresses; avoid touching the bus here.
	d.addrTemp = core.CapAddr{Domain: "env", Kind: string(types.KindTemperature), Name: d.id}
	d.addrHum = core.CapAddr{Domain: "env", Kind: string(types.KindHumidity), Name: d.id}
	// Provide the address without doing I²C; Configure() will run inside the job.
	d.drv.Address = d.addr
	return nil
}

func (d *Device) Close() error {
	if d.reg != nil {
		d.reg.ReleaseI2C(d.id, core.ResourceID(d.bus))
	}
	return nil
}

// Ensure our job type satisfies the interface at compile time.
var _ core.I2CJob = (*aht20ReadJob)(nil)

// aht20ReadJob is a reusable, closure-free job value.
type aht20ReadJob struct{ d *Device }

func (j *aht20ReadJob) Run(bus core.I2CBus) error {
	d := j.d

	// Bind the hot shim to the worker's per-job bus and configure before use.
	d.hot.Bind(bus)
	// Safe to call here (inside worker context). Also idempotent.
	d.drv.Configure(aht20.Config{
		Address:        d.addr,
		PollInterval:   15 * time.Millisecond,
		CollectTimeout: 250 * time.Millisecond,
		TriggerHint:    80 * time.Millisecond,
	})

	start := time.Now().UnixNano()
	if err := d.drv.Read(); err != nil {
		d.emitErr(string(errcode.MapDriverErr(err)), start)
		return nil
	}

	// Fixed-point conversions with bounds.
	decic := d.drv.DeciCelsius()
	decic = int32(mathx.Clamp(int(decic), -32768, 32767))
	rhx100 := d.drv.DeciRelHumidity() * 10
	rhx100 = int32(mathx.Clamp(int(rhx100), 0, 10000))

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
	return nil
}

func (d *Device) emitErr(code string, t0 int64) {
	d.pub.Emit(core.Event{Addr: d.addrTemp, Err: code, TS: t0})
	d.pub.Emit(core.Event{Addr: d.addrHum, Err: code, TS: t0})
}

func (d *Device) Control(_ core.CapAddr, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "read":
		if !d.i2c.TryEnqueueJob(d.jobRead) {
			return core.EnqueueResult{OK: false, Error: errcode.Busy}, nil
		}
		return core.EnqueueResult{OK: true}, nil
	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

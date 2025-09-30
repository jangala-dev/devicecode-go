package shtc3dev

import (
	"context"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/drvshim"
	"devicecode-go/types"
	"devicecode-go/x/mathx"

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
	own, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}

	d := &Device{
		id:  in.ID,
		bus: p.Bus,
		i2c: own,
		pub: in.Res.Pub,
		reg: in.Res.Reg,
	}

	// Persistent hot-swappable I2C shim and driver instance.
	d.hot = &drvshim.HotI2C{}
	d.drv = shtc3.New(d.hot)

	// Reusable, closure-free job object.
	d.jobRead = &shtc3ReadJob{d: d}

	return d, nil
}

type Device struct {
	id  string
	bus string

	i2c core.I2COwner
	pub core.EventEmitter
	reg core.ResourceRegistry

	// Persisted bus shim and driver (no per-call allocations).
	hot     *drvshim.HotI2C
	drv     shtc3.Device
	jobRead *shtc3ReadJob

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

// Compile-time check.
var _ core.I2CJob = (*shtc3ReadJob)(nil)

// Reusable, closure-free job value.
type shtc3ReadJob struct{ d *Device }

func (j *shtc3ReadJob) Run(bus core.I2CBus) error {
	d := j.d

	// Bind the hot shim to the worker's per-job bus.
	d.hot.Bind(bus)

	// Wake, read, sleep within the worker context.
	_ = d.drv.WakeUp()
	defer func() { _ = d.drv.Sleep() }()

	t0 := time.Now().UnixNano()
	tmc, rhx100, err := d.drv.ReadTemperatureHumidity()
	if err != nil {
		d.emitErr(string(errcode.MapDriverErr(err)), t0)
		return nil
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
	return nil
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

func (d *Device) emitErr(code string, t0 int64) {
	d.pub.Emit(core.Event{Addr: d.addrTemp, Err: code, TS: t0})
	d.pub.Emit(core.Event{Addr: d.addrHum, Err: code, TS: t0})
}

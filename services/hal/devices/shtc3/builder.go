package shtc3dev

import (
	"context"
	"errors"
	"time"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/drvshim"
	"devicecode-go/types"

	"tinygo.org/x/drivers/shtc3"
)

func init() { core.RegisterBuilder("shtc3", builder{}) }

type Params struct {
	Bus string // e.g. "i2c0"
	// Address is fixed at 0x70 for SHTC3; keep field if variants arise.
}

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok || p.Bus == "" {
		return nil, errors.New("invalid_params")
	}
	own, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}
	bus := drvshim.NewI2C(own).WithTimeout(50)
	drv := shtc3.New(bus)

	return &Device{
		id:  in.ID,
		bus: p.Bus,
		i2c: own,
		drv: drv,
		pub: in.Res.Pub,
		q:   make(chan string, 4),
	}, nil
}

type Device struct {
	id  string
	bus string

	i2c core.I2COwner
	drv shtc3.Device
	pub core.EventEmitter

	q    chan string
	stop chan struct{}
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{
		{
			Kind: types.KindTemperature,
			Info: types.Info{
				SchemaVersion: 1,
				Driver:        "shtc3",
				Detail:        types.TemperatureInfo{Sensor: "shtc3", Addr: 0x70, Bus: d.bus},
			},
		},
		{
			Kind: types.KindHumidity,
			Info: types.Info{
				SchemaVersion: 1,
				Driver:        "shtc3",
				Detail:        types.HumidityInfo{Sensor: "shtc3", Addr: 0x70, Bus: d.bus},
			},
		},
	}
}

func (d *Device) Init(ctx context.Context) error {
	d.stop = make(chan struct{})
	_ = d.drv.WakeUp()
	go d.loop()
	return nil
}

func (d *Device) Close() error {
	_ = d.drv.Sleep()
	close(d.stop)
	return nil
}

func (d *Device) Control(kind types.Kind, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "read":
		select {
		case d.q <- "read":
			return core.EnqueueResult{OK: true}, nil
		default:
			return core.EnqueueResult{OK: false, Error: "busy"}, nil
		}
	default:
		return core.EnqueueResult{OK: false, Error: "unsupported"}, nil
	}
}

func (d *Device) loop() {
	for {
		select {
		case <-d.q:
			d.handleRead()
		case <-d.stop:
			return
		}
	}
}

func (d *Device) handleRead() {
	t0 := time.Now().UnixMilli()
	tmc, rhx100, err := d.drv.ReadTemperatureHumidity()
	if err != nil {
		d.emitErr(err.Error(), t0)
		return
	}
	// shtc3 returns milli°C and hundredths of %RH.
	decic := tmc / 100 // milli°C -> deci°C
	if decic > 32767 {
		decic = 32767
	}
	if decic < -32768 {
		decic = -32768
	}
	if rhx100 < 0 {
		rhx100 = 0
	}
	if rhx100 > 10000 {
		rhx100 = 10000
	}
	ts := time.Now().UnixMilli()
	d.pub.Emit(core.Event{
		DevID:   d.id,
		Kind:    types.KindTemperature,
		Payload: types.TemperatureValue{DeciC: int16(decic)},
		TSms:    ts,
	})
	d.pub.Emit(core.Event{
		DevID:   d.id,
		Kind:    types.KindHumidity,
		Payload: types.HumidityValue{RHx100: uint16(rhx100)},
		TSms:    ts,
	})
}

func (d *Device) emitErr(code string, t0 int64) {
	d.pub.Emit(core.Event{
		DevID: d.id,
		Kind:  types.KindTemperature,
		Err:   code,
		TSms:  t0,
	})
	d.pub.Emit(core.Event{
		DevID: d.id,
		Kind:  types.KindHumidity,
		Err:   code,
		TSms:  t0,
	})
}

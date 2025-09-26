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
	return &Device{
		id:  in.ID,
		bus: p.Bus,
		i2c: own,
		pub: in.Res.Pub,
	}, nil
}

type Device struct {
	id  string
	bus string

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
				Driver:        "shtc3",
				Detail:        types.TemperatureInfo{Sensor: "shtc3", Addr: 0x70, Bus: d.bus},
			},
		},
		{
			Domain: "env",
			Kind:   types.KindHumidity,
			Name:   d.id,
			Info: types.Info{
				SchemaVersion: 1,
				Driver:        "shtc3",
				Detail:        types.HumidityInfo{Sensor: "shtc3", Addr: 0x70, Bus: d.bus},
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
			// Construct a driver bound to the worker's bus each time.
			drv := shtc3.New(drvshim.NewI2CFromBus(bus))

			// Wake, read, sleep in one job to keep the bus time bounded.
			_ = drv.WakeUp()
			defer func() { _ = drv.Sleep() }()

			t0 := time.Now().UnixMilli()
			tmc, rhx100, err := drv.ReadTemperatureHumidity()
			if err != nil {
				d.emitErr(err.Error(), t0)
				return nil
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

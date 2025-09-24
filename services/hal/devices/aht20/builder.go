package aht20dev

import (
	"context"
	"errors"
	"time"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/drvshim"
	"devicecode-go/types"

	// Adjust this path to where you store the AHT20 driver you pasted.
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
	if !ok || p.Bus == "" || p.Addr == 0 {
		return nil, errors.New("invalid_params")
	}
	own, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}
	bus := drvshim.NewI2C(own).WithTimeout(50)
	d := &Device{
		id:   in.ID,
		bus:  p.Bus,
		addr: p.Addr,
		i2c:  own,
		pub:  in.Res.Pub,
		q:    make(chan string, 4),
	}
	drv := aht20.New(bus)
	d.drv = &drv
	return d, nil
}

type Device struct {
	id   string
	bus  string
	addr uint16

	i2c core.I2COwner
	drv *aht20.Device
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
				Driver:        "aht20",
				Detail:        types.TemperatureInfo{Sensor: "aht20", Addr: d.addr, Bus: d.bus},
			},
		},
		{
			Kind: types.KindHumidity,
			Info: types.Info{
				SchemaVersion: 1,
				Driver:        "aht20",
				Detail:        types.HumidityInfo{Sensor: "aht20", Addr: d.addr, Bus: d.bus},
			},
		},
	}
}

func (d *Device) Init(ctx context.Context) error {
	d.stop = make(chan struct{})
	// Configure with sane defaults (the driver tolerates repeated calls).
	d.drv.Configure()
	go d.loop()
	return nil
}

func (d *Device) Close() error {
	close(d.stop)
	// The bus owner is long-lived; nothing to release beyond matching your API.
	return nil
}

// Non-blocking; accepts "read" for either kind.
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
	start := time.Now().UnixMilli()
	// Full cycle: trigger then bounded polling handled inside Read().
	if err := d.drv.Read(); err != nil {
		d.emitErr(err.Error(), start)
		return
	}
	// Convert to fixed-point types.
	// Driver exposes DeciCelsius() and DeciRelHumidity() from cached sample.
	decic := d.drv.DeciCelsius()
	if decic > 32767 {
		decic = 32767
	}
	if decic < -32768 {
		decic = -32768
	}
	rhx100 := d.drv.DeciRelHumidity() * 10 // Deci% => hundredths of %
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

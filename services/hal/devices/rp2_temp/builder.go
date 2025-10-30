package rp2_temp

import (
	"context"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

func init() { core.RegisterBuilder("rp2_temp", builder{}) }

type Params struct {
	Domain string // REQUIRED
	Name   string // REQUIRED
}

type builder struct{}

// narrow, package-local capability: implemented by rp2 provider only
type dieTempReader interface {
	ReadOnDieMilliC() int32
}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok || p.Domain == "" || p.Name == "" {
		return nil, errcode.InvalidParams
	}
	// Feature-detect: only works on providers that implement the method.
	rdr, ok := in.Res.Reg.(dieTempReader)
	if !ok {
		return nil, errcode.Unsupported
	}
	return &Device{
		id:   in.ID,
		pub:  in.Res.Pub,
		dom:  p.Domain,
		name: p.Name,
		read: rdr.ReadOnDieMilliC,
	}, nil
}

type Device struct {
	id   string
	pub  core.EventEmitter
	dom  string
	name string

	addr core.CapAddr
	read func() int32 // provider-injected milli-celsius reader
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{{
		Domain: d.dom,
		Kind:   types.KindTemperature,
		Name:   d.name,
		Info: types.Info{
			SchemaVersion: 1,
			Driver:        "rp2_temp",
			Detail:        types.TemperatureInfo{Sensor: "rp2040_internal"},
		},
	}}
}

func (d *Device) Init(ctx context.Context) error {
	d.addr = core.CapAddr{Domain: d.dom, Kind: types.KindTemperature, Name: d.name}
	return nil
}

func (d *Device) Close() error { return nil }

func (d *Device) Control(_ core.CapAddr, verb string, _ any) (core.EnqueueResult, error) {
	if verb != "read" {
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
	// Synchronous, fast, and non-contentious: no goroutine required.
	mc := d.read()                // milli-celsius
	decic := mc / 100             // deci-celsius
	const tMin, tMax = -400, 1250 // −40.0..+125.0 °C
	if decic < tMin || decic > tMax {
		_ = d.pub.Emit(core.Event{Addr: d.addr, Err: "invalid_sample"})
		return core.EnqueueResult{OK: true}, nil
	}
	_ = d.pub.Emit(core.Event{
		Addr:    d.addr,
		Payload: types.TemperatureValue{DeciC: int16(decic)},
	})
	return core.EnqueueResult{OK: true}, nil
}

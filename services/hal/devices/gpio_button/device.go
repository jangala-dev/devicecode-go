package gpio_button

import (
	"context"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

type Device struct {
	id     string
	pinN   int
	gpio   core.GPIOHandle
	invert bool

	pub core.EventEmitter
	reg core.ResourceRegistry

	dom  string
	name string
	a    core.CapAddr

	debounce time.Duration
	es       core.GPIOEdgeStream
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{{
		Domain: d.dom,
		Kind:   types.KindButton,
		Name:   d.name,
		Info:   types.Info{SchemaVersion: 1, Driver: "gpio_button", Detail: types.ButtonInfo{Pin: d.pinN}},
	}}
}

func (d *Device) Init(ctx context.Context) error {
	d.a = core.CapAddr{Domain: d.dom, Kind: types.KindButton, Name: d.name}

	// Publish initial value.
	lvl := d.gpio.Get()
	pressed := d.logicalPressed(lvl)
	d.pub.Emit(core.Event{
		Addr:    d.a,
		Payload: types.ButtonValue{Pressed: pressed},
	})

	es, err := d.reg.SubscribeGPIOEdges(d.id, d.pinN, core.EdgeBoth, d.debounce, 8)
	if err != nil {
		d.pub.Emit(core.Event{Addr: d.a, Err: "edge_sub_failed"})
		return nil
	}
	d.es = es
	go d.edgeLoop()
	return nil
}

func (d *Device) Close() error {
	if d.es != nil {
		d.es.Close()
		d.reg.UnsubscribeGPIOEdges(d.id, d.pinN)
	}
	d.reg.ReleasePin(d.id, d.pinN)
	return nil
}

func (d *Device) Control(_ core.CapAddr, verb string, _ any) (core.EnqueueResult, error) {
	switch verb {
	case "read":
		pressed := d.logicalPressed(d.gpio.Get())
		_ = d.pub.Emit(core.Event{Addr: d.a, Payload: types.ButtonValue{Pressed: pressed}})
		return core.EnqueueResult{OK: true}, nil
	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

func (d *Device) edgeLoop() {
	for ev := range d.es.Events() {
		pressed := d.logicalPressed(ev.Level)
		tag := "released"
		if pressed {
			tag = "pressed"
		}
		_ = d.pub.Emit(core.Event{Addr: d.a, EventTag: tag})
		_ = d.pub.Emit(core.Event{Addr: d.a, Payload: types.ButtonValue{Pressed: pressed}})
	}
}

func (d *Device) logicalPressed(level bool) bool {
	if d.invert {
		return !level
	}
	return level
}

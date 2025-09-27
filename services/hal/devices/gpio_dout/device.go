package gpio_dout

import (
	"context"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
	"devicecode-go/x/timex"
)

type Params struct {
	Pin       int
	ActiveLow bool
	Initial   bool
	Domain    string
	Name      string
}

type Role int

const (
	RoleLED Role = iota
	RoleSwitch
)

type Device struct {
	id        string
	pin       core.GPIOHandle
	pinN      int
	activeLow bool
	pub       core.EventEmitter
	role      Role
	domain    string
	name      string
	initial   bool
	// derived address for the single capability
	addr core.CapAddr
}

func New(role Role, id string, p Params, h core.GPIOHandle, pub core.EventEmitter) *Device {
	d := &Device{
		id:        id,
		pin:       h,
		pinN:      p.Pin,
		activeLow: p.ActiveLow,
		pub:       pub,
		role:      role,
		domain:    p.Domain,
		name:      p.Name,
		initial:   p.Initial,
	}
	if d.name == "" {
		d.name = id
	}
	if d.domain == "" {
		switch role {
		case RoleSwitch:
			d.domain = "power"
		default:
			d.domain = "io"
		}
	}
	kind := string(types.KindLED)
	if role == RoleSwitch {
		kind = string(types.KindSwitch)
	}
	d.addr = core.CapAddr{Domain: d.domain, Kind: kind, Name: d.name}
	return d
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	switch d.role {
	case RoleSwitch:
		return []core.CapabilitySpec{{
			Domain: d.domain,
			Kind:   types.KindSwitch,
			Name:   d.name,
			Info: types.Info{
				SchemaVersion: 1,
				Driver:        "gpio_dout",
				Detail:        types.SwitchInfo{Pin: d.pin.Number()},
			},
		}}
	default:
		return []core.CapabilitySpec{{
			Domain: d.domain,
			Kind:   types.KindLED,
			Name:   d.name,
			Info: types.Info{
				SchemaVersion: 1,
				Driver:        "gpio_dout",
				Detail:        types.LEDInfo{Pin: d.pin.Number()},
			},
		}}
	}
}

func (d *Device) Init(ctx context.Context) error {
	level := d.initial
	if d.activeLow {
		level = !level
	}
	if err := d.pin.ConfigureOutput(level); err != nil {
		return err
	}
	d.emitValueNow()
	return nil
}

func (d *Device) Close() error { return nil }

func (d *Device) Control(_ core.CapAddr, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "set":
		switch d.role {
		case RoleSwitch:
			p, ok := payload.(types.SwitchSet)
			if !ok {
				return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
			}
			d.setLogical(p.On)
		default:
			p, ok := payload.(types.LEDSet)
			if !ok {
				return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
			}
			d.setLogical(p.Level)
		}
		d.emitValueNow()
		return core.EnqueueResult{OK: true}, nil
	case "toggle":
		d.setLogical(!d.getLogical())
		d.emitValueNow()
		return core.EnqueueResult{OK: true}, nil
	case "read":
		d.emitValueNow()
		return core.EnqueueResult{OK: true}, nil
	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

func (d *Device) setLogical(on bool) {
	level := on
	if d.activeLow {
		level = !level
	}
	d.pin.Set(level)
}

func (d *Device) getLogical() bool {
	level := d.pin.Get()
	if d.activeLow {
		level = !level
	}
	return level
}

func (d *Device) emitValueNow() {
	ts := timex.NowMs()
	switch d.role {
	case RoleSwitch:
		_ = d.pub.Emit(core.Event{
			Addr:    d.addr,
			Payload: types.SwitchValue{On: d.getLogical()},
			TSms:    ts,
		})
	default:
		var v uint8
		if d.getLogical() {
			v = 1
		}
		_ = d.pub.Emit(core.Event{
			Addr:    d.addr,
			Payload: types.LEDValue{Level: v},
			TSms:    ts,
		})
	}
}

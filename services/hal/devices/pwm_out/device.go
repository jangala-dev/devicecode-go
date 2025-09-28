// services/hal/devices/pwm_out/device.go
package pwm_out

import (
	"context"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

type Device struct {
	id   string
	pin  int
	pwm  core.PWMHandle
	pub  core.EventEmitter
	dom  string
	name string
	freq uint64
	top  uint16

	addr core.CapAddr
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	return []core.CapabilitySpec{{
		Domain: d.dom,
		Kind:   types.KindPWM,
		Name:   d.name,
		Info: types.Info{
			SchemaVersion: 1,
			Driver:        "pwm_out",
			Detail:        types.PWMInfo{Pin: d.pin, FreqHz: d.freq, Top: d.top}, // add in types
		},
	}}
}

func (d *Device) Init(ctx context.Context) error {
	_ = d.pwm.Configure(d.freq, d.top) // map provider error to degraded in control if needed
	d.addr = core.CapAddr{Domain: d.dom, Kind: string(types.KindPWM), Name: d.name}
	// emit initial value (0)
	d.pub.Emit(core.Event{Addr: d.addr, Payload: types.PWMValue{Level: 0}, TS: time.Now().UnixNano()})
	return nil
}

func (d *Device) Close() error { return nil }

func (d *Device) Control(_ core.CapAddr, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "set":
		p, ok := payload.(types.PWMSet)
		if !ok {
			return core.EnqueueResult{OK: false}, nil
		}
		d.pwm.Set(p.Level)
		d.pub.Emit(core.Event{Addr: d.addr, Payload: types.PWMValue{Level: p.Level}, TS: time.Now().UnixNano()})
		return core.EnqueueResult{OK: true}, nil
	case "ramp":
		p, ok := payload.(types.PWMRamp) // e.g. {To uint16, DurationMs uint32, Steps uint16, Mode uint8}
		if !ok {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}
		started := d.pwm.Ramp(p.To, p.DurationMs, p.Steps, core.PWMRampMode(p.Mode))
		if !started {
			return core.EnqueueResult{OK: false, Error: errcode.Busy}, nil
		}
		return core.EnqueueResult{OK: true}, nil
	case "stop_ramp":
		d.pwm.StopRamp()
		return core.EnqueueResult{OK: true}, nil
	default:
		return core.EnqueueResult{OK: false}, nil
	}
}

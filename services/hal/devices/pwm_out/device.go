package pwm_out

import (
	"context"
	"time"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

type Device struct {
	id        string
	pin       int
	pwm       core.PWMHandle
	pub       core.EventEmitter
	reg       core.ResourceRegistry
	dom       string
	name      string
	freq      uint64
	top       uint16
	activeLow bool
	initial   uint16 // initial *logical* level
	addr      core.CapAddr
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
			Detail: types.PWMInfo{
				Pin:       d.pin,
				FreqHz:    d.freq,
				Top:       d.top,
				ActiveLow: d.activeLow,
				Initial:   d.initial,
			},
		},
	}}
}

// --- helpers: clamp + logical<->physical mapping (invert if ActiveLow) ---

func (d *Device) clamp(lvl uint16) uint16 {
	if d.top == 0 {
		return 0
	}
	if lvl > d.top {
		return d.top
	}
	return lvl
}

func (d *Device) toPhys(logical uint16) uint16 {
	l := d.clamp(logical)
	if !d.activeLow {
		return l
	}
	return d.top - l
}

func (d *Device) toLogical(phys uint16) uint16 {
	p := phys
	if d.top != 0 && p > d.top {
		p = d.top
	}
	if !d.activeLow {
		return p
	}
	return d.top - p
}

func (d *Device) Init(ctx context.Context) error {
	if err := d.pwm.Configure(d.freq, d.top); err != nil {
		d.pub.Emit(core.Event{
			Addr: core.CapAddr{Domain: d.dom, Kind: string(types.KindPWM), Name: d.name},
			TS:   time.Now().UnixNano(),
			Err:  string(errcode.MapDriverErr(err)),
		})
		return nil
	}

	d.addr = core.CapAddr{Domain: d.dom, Kind: string(types.KindPWM), Name: d.name}

	// Apply initial logical level (default 0) as *physical* output.
	initialLog := d.clamp(d.initial)
	d.pwm.Set(d.toPhys(initialLog))

	// Publish current *logical* value so the rest of the system sees 0..Top.
	d.pub.Emit(core.Event{
		Addr:    d.addr,
		Payload: types.PWMValue{Level: initialLog},
		TS:      time.Now().UnixNano(),
	})
	return nil
}

// Close stops any active ramp and releases the claimed pin.
func (d *Device) Close() error {
	if d.pwm != nil {
		d.pwm.StopRamp()
	}
	if d.reg != nil {
		d.reg.ReleasePin(d.id, d.pin)
	}
	return nil
}

func (d *Device) Control(_ core.CapAddr, method string, payload any) (core.EnqueueResult, error) {
	switch method {
	case "set":
		p, ok := payload.(types.PWMSet)
		if !ok {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}
		logical := d.clamp(p.Level)
		d.pwm.Set(d.toPhys(logical))
		d.pub.Emit(core.Event{
			Addr:    d.addr,
			Payload: types.PWMValue{Level: logical}, // publish logical
			TS:      time.Now().UnixNano(),
		})
		return core.EnqueueResult{OK: true}, nil

	case "ramp":
		p, ok := payload.(types.PWMRamp) // {To uint16, DurationMs uint32, Steps uint16, Mode uint8}
		if !ok {
			return core.EnqueueResult{OK: false, Error: errcode.InvalidPayload}, nil
		}
		toPhys := d.toPhys(d.clamp(p.To)) // invert target if active-low
		started := d.pwm.Ramp(toPhys, p.DurationMs, p.Steps, core.PWMRampMode(p.Mode))
		if !started {
			return core.EnqueueResult{OK: false, Error: errcode.Busy}, nil
		}
		// (Optional) we can emit a “ramping” event here if we like, using logical target p.To
		return core.EnqueueResult{OK: true}, nil

	case "stop_ramp":
		d.pwm.StopRamp()
		return core.EnqueueResult{OK: true}, nil

	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

// services/hal/devices/ltc4015/device.go
package ltc4015dev

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"devicecode-go/drivers/ltc4015"
	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"

	"tinygo.org/x/drivers"
)

type Device struct {
	id   string
	aBat core.CapAddr // power/battery/<name>
	aChg core.CapAddr // power/charger/<name>
	aTmp core.CapAddr // power/charger/<name>/temperature

	res  core.Resources
	i2c  drivers.I2C
	pin  int
	gpio core.GPIOHandle
	es   core.GPIOEdgeStream

	// Worker orchestration
	ctx    context.Context
	cancel context.CancelFunc
	dev    *ltc4015.Device
	reqCh  chan request
	done   chan struct{}
	alive  atomic.Bool // guards enqueue/timers after stop

	// Retry timer for SMBALERT# re-service
	retryTimer *time.Timer

	// Last configured windows (for state-aware opposite-edge re-arming)
	lastVinLo, lastVinHi           int32
	lastVsysLo, lastVsysHi         int32
	lastVbatLoCell, lastVbatHiCell int32
	lastNTCHi, lastNTCLo           uint16

	// Desired alert sources (user intent). Auto re-arming always applies.
	desiredLimit  ltc4015.LimitEnable
	desiredState  ltc4015.ChargerStateEnable
	desiredStatus ltc4015.ChargeStatusEnable

	params Params
}

type opCode uint8

const (
	opRead opCode = iota
	opConfigure
	opServiceAlert
	opStop
)

type request struct {
	op  opCode
	arg any
}

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	bi := types.BatteryInfo{
		Cells:      d.params.Cells,
		Chem:       d.params.Chem,
		RSNSB_uOhm: d.params.RSNSB_uOhm,
		Bus:        d.params.Bus,
		Addr:       d.params.Addr,
	}
	ci := types.ChargerInfo{
		RSNSI_uOhm: d.params.RSNSI_uOhm,
		Bus:        d.params.Bus,
		Addr:       d.params.Addr,
	}
	return []core.CapabilitySpec{
		{
			Domain: d.aBat.Domain, Kind: types.KindBattery, Name: d.aBat.Name,
			Info: types.Info{SchemaVersion: 1, Driver: "ltc4015", Detail: bi},
		},
		{
			Domain: d.aChg.Domain, Kind: types.KindCharger, Name: d.aChg.Name,
			Info: types.Info{SchemaVersion: 1, Driver: "ltc4015", Detail: ci},
		},
		{
			Domain: d.aTmp.Domain, Kind: types.KindTemperature, Name: d.aTmp.Name,
			Info: types.Info{
				SchemaVersion: 1, Driver: "ltc4015",
				Detail: types.TemperatureInfo{Sensor: "ntc@ltc4015", Addr: d.params.Addr, Bus: d.params.Bus},
			},
		},
	}
}

func (d *Device) Init(ctx context.Context) error {
	// Initialise worker channels and context *before* spawning any goroutines that call enqueue.
	d.reqCh = make(chan request, 8)
	d.done = make(chan struct{})
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.alive.Store(true)

	// Subscribe to SMBALERT# falling edges (best-effort).
	if es, err := d.res.Reg.SubscribeGPIOEdges(d.id, d.pin, core.EdgeFalling, 2*time.Millisecond, 8); err == nil {
		d.es = es
	} else {
		// Degraded status if IRQ subscription fails; polling via worker will still operate.
		_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: "alert_subscribe_failed"})
		_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: "alert_subscribe_failed"})
	}

	// Advertise initial state as degraded until the first good sample.
	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: "initialising"})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: "initialising"})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aTmp, Err: "initialising"})

	go d.worker(d.ctx)

	// Apply any boot actions declared in Params via the standard control path.
	if d.params.Boot != nil {
		for _, a := range d.params.Boot {
			// Use the charger capability as the target; Control ignores the CapAddr value.
			_, _ = d.Control(d.aChg, a.Verb, a.Payload)
		}
	}

	// Seed a first sample.
	d.enqueue(opRead, nil)
	return nil
}

func (d *Device) Close() error {
	// Prevent further enqueues and stop any outstanding retry timer early.
	d.alive.Store(false)
	if d.retryTimer != nil {
		if !d.retryTimer.Stop() {
			select {
			case <-d.retryTimer.C:
			default:
			}
		}
		d.retryTimer = nil
	}

	// If the worker was never started, just cancel and return.
	if d.reqCh == nil || d.done == nil {
		if d.cancel != nil {
			d.cancel()
		}
		return nil
	}

	// Prefer an orderly stop; fallback to cancellation if the queue is full.
	select {
	case d.reqCh <- request{op: opStop}:
	default:
		if d.cancel != nil {
			d.cancel()
		}
	}

	// Wait for worker exit with a bounded timeout; ensure cancellation if slow.
	t := time.NewTimer(300 * time.Millisecond)
	defer t.Stop()
	select {
	case <-d.done:
		// ok
	case <-t.C:
		if d.cancel != nil {
			d.cancel()
		}
		<-d.done
	}
	return nil
}

// ---- Controls ----

func (d *Device) Control(_ core.CapAddr, verb string, payload any) (core.EnqueueResult, error) {
	switch verb {
	case "read":
		d.enqueue(opRead, nil)
		return core.EnqueueResult{OK: true}, nil

	case "configure":
		cfg, code := core.As[types.ChargerConfigure](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		d.enqueue(opConfigure, cfg)
		return core.EnqueueResult{OK: true}, nil

	// Convenience verbs -> configure partials
	case "enable":
		t := true
		d.enqueue(opConfigure, types.ChargerConfigure{Enable: &t})
		return core.EnqueueResult{OK: true}, nil
	case "disable":
		f := false
		d.enqueue(opConfigure, types.ChargerConfigure{Enable: &f})
		return core.EnqueueResult{OK: true}, nil

	case "set_vin_window":
		p, code := core.As[types.VinWindowSet](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		lo, hi := p.Lo_mV, p.Hi_mV
		d.enqueue(opConfigure, types.ChargerConfigure{VinLo_mV: &lo, VinHi_mV: &hi})
		return core.EnqueueResult{OK: true}, nil

	case "set_vbat_window":
		p, code := core.As[types.VbatWindowSet](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		lo, hi := p.Lo_mVPerCell, p.Hi_mVPerCell
		d.enqueue(opConfigure, types.ChargerConfigure{VbatLo_mVPerCell: &lo, VbatHi_mVPerCell: &hi})
		return core.EnqueueResult{OK: true}, nil

	case "set_vsys_window":
		p, code := core.As[types.VsysWindowSet](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		lo, hi := p.Lo_mV, p.Hi_mV
		d.enqueue(opConfigure, types.ChargerConfigure{VsysLo_mV: &lo, VsysHi_mV: &hi})
		return core.EnqueueResult{OK: true}, nil

	case "set_iin_high":
		p, code := core.As[types.CurrentMA](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		v := p.MilliA
		d.enqueue(opConfigure, types.ChargerConfigure{IinHigh_mA: &v})
		return core.EnqueueResult{OK: true}, nil

	case "set_ibat_low":
		p, code := core.As[types.CurrentMA](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		v := p.MilliA
		d.enqueue(opConfigure, types.ChargerConfigure{IbatLow_mA: &v})
		return core.EnqueueResult{OK: true}, nil

	case "set_die_temp_high":
		p, code := core.As[types.TempMilliC](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		v := p.MilliC
		d.enqueue(opConfigure, types.ChargerConfigure{DieTempHigh_mC: &v})
		return core.EnqueueResult{OK: true}, nil

	case "set_ntc_ratio_window":
		p, code := core.As[types.NTCRatioWindowRaw](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		hi, lo := p.Hi, p.Lo
		d.enqueue(opConfigure, types.ChargerConfigure{NTCRatioHi: &hi, NTCRatioLo: &lo})
		return core.EnqueueResult{OK: true}, nil

	case "set_vin_uvcl":
		p, code := core.As[types.VoltageMV](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		v := p.MilliV
		d.enqueue(opConfigure, types.ChargerConfigure{VinUVCL_mV: &v})
		return core.EnqueueResult{OK: true}, nil

	case "set_input_limit":
		p, code := core.As[types.CurrentMA](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		v := p.MilliA
		d.enqueue(opConfigure, types.ChargerConfigure{IinLimit_mA: &v})
		return core.EnqueueResult{OK: true}, nil

	case "set_charge_target":
		p, code := core.As[types.CurrentMA](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		v := p.MilliA
		d.enqueue(opConfigure, types.ChargerConfigure{IChargeTarget_mA: &v})
		return core.EnqueueResult{OK: true}, nil

	case "set_bsr_high":
		p, code := core.As[types.ResistanceMicroOhmPerCell](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		u := p.MicroOhmPerCell
		d.enqueue(opConfigure, types.ChargerConfigure{BSRHigh_uOhmPerCell: &u})
		return core.EnqueueResult{OK: true}, nil

	case "alerts_mask":
		m, code := core.As[types.ChargerAlertMask](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		d.enqueue(opConfigure, types.ChargerConfigure{AlertMask: &m})
		return core.EnqueueResult{OK: true}, nil

	case "config_bits_update":
		p, code := core.As[types.ChargerConfigBitsUpdate](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		d.enqueue(opConfigure, types.ChargerConfigure{CfgSet: &p.Set, CfgClear: &p.Clear})
		return core.EnqueueResult{OK: true}, nil

	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

// ---- Worker ----

// enqueue posts a request without blocking the caller.
func (d *Device) enqueue(op opCode, arg any) {
	if !d.alive.Load() || d.ctx == nil {
		return
	}
	select {
	case <-d.ctx.Done():
		return
	default:
	}
	select {
	case d.reqCh <- request{op: op, arg: arg}:
	default:
	}
}

func (d *Device) worker(ctx context.Context) {
	defer close(d.done)

	// Construct driver and configure baseline.
	cfg := ltc4015.Config{
		Address:        d.params.Addr,
		RSNSB_uOhm:     d.params.RSNSB_uOhm,
		RSNSI_uOhm:     d.params.RSNSI_uOhm,
		Cells:          d.params.Cells,
		Chem:           ltc4015.ChemUnknown, // validated below against Params.Chem
		QCountPrescale: d.params.QCountPrescale,
	}
	drv, err := ltc4015.NewAuto(d.i2c, cfg)
	if err != nil {
		d.errBoth("ltc4015_init_failed", err)
		d.cleanup()
		return
	}
	// Safety rails: hardware variant/cell-strap must match explicit Params.
	if exp, ok := chemParamToExpect(d.params.Chem); !ok {
		d.errBoth("ltc4015_strapping_mismatch", ltc4015.ErrUnknownChemParam)
		d.cleanup()
		return
	} else if err := drv.ValidateAgainst(exp, d.params.Cells); err != nil {
		d.errBoth("ltc4015_strapping_mismatch", err)
		d.cleanup()
		return
	}
	if err := drv.Configure(cfg); err != nil {
		// Emit degraded status on both capabilities and stop; without configuration we cannot proceed safely.
		d.errBoth("configure_failed", err)
		d.cleanup()
		return
	}
	_ = drv.SetConfigBits(ltc4015.ForceMeasSysOn | ltc4015.EnableQCount)
	d.dev = drv

	d.desiredLimit = 0
	d.desiredState = d.desiredChargerStateMask()
	d.desiredStatus = d.desiredChargeStatusMask()

	// If line already asserted, service now.
	if d.dev.AlertActive(func() bool { return d.gpio.Get() }) {
		d.serviceAlertBatch()
	}

	// Helper to expose timer channel safely to select.
	retryC := func() <-chan time.Time {
		if d.retryTimer == nil {
			return nil
		}
		return d.retryTimer.C
	}
	// Route edge events through the worker to avoid a separate goroutine.
	var evCh <-chan core.GPIOEdgeEvent
	if d.es != nil {
		evCh = d.es.Events()
	}

	for {
		select {
		case <-ctx.Done():
			d.alive.Store(false)
			d.cleanup()
			return

		case <-evCh:
			// SMBALERT# edge observed; drain/handle a batch.
			d.serviceAlertBatch()

		case <-retryC():
			// Timer fired to revisit a still-asserted ALERT# condition.
			d.enqueue(opServiceAlert, nil)

		case req := <-d.reqCh:
			switch req.op {
			case opRead:
				d.sampleAndPublish()

			case opConfigure:
				if c, _ := req.arg.(types.ChargerConfigure); (c != types.ChargerConfigure{}) {
					d.applyConfigure(c)
					// After any configure: re-arm (opposite edge) then publish.
					d.rearm()
					d.sampleAndPublish()
				}

			case opServiceAlert:
				d.serviceAlertBatch()

			case opStop:
				d.alive.Store(false)
				d.cleanup()
				return
			}
		}
	}
}

func (d *Device) cleanup() {
	// Close edge stream and release claims.
	if d.es != nil {
		d.es.Close()
		d.res.Reg.UnsubscribeGPIOEdges(d.id, d.pin)
		d.es = nil
	}
	d.res.Reg.ReleasePin(d.id, d.pin)
	d.res.Reg.ReleaseI2C(d.id, core.ResourceID(d.params.Bus))
	if d.cancel != nil {
		d.cancel()
	}
	// Stop retry timer safely after resources are released.
	if d.retryTimer != nil {
		if !d.retryTimer.Stop() {
			select {
			case <-d.retryTimer.C:
			default:
			}
		}
		d.retryTimer = nil
	}
}

// ---------- Configure application ----------

func (d *Device) applyConfigure(c types.ChargerConfigure) {
	// CONFIG bits (set/clear/update)
	if c.CfgSet != nil || c.CfgClear != nil {
		var set, clr ltc4015.ConfigBits
		if c.CfgSet != nil {
			set = ltc4015.ConfigBits(*c.CfgSet)
		}
		if c.CfgClear != nil {
			clr = ltc4015.ConfigBits(*c.CfgClear)
		}
		_ = d.dev.UpdateConfig(set, clr)
	}
	if c.Enable != nil {
		if *c.Enable {
			_ = d.dev.ClearConfigBits(ltc4015.SuspendCharger)
		} else {
			_ = d.dev.SetConfigBits(ltc4015.SuspendCharger)
		}
	}
	if c.LeadAcidTempComp != nil {
		if d.dev.Chem() == ltc4015.ChemLeadAcid {
			if *c.LeadAcidTempComp {
				_ = d.dev.SetChargerConfigBits(ltc4015.EnLeadAcidTempComp)
			} else {
				_ = d.dev.ClearChargerConfigBits(ltc4015.EnLeadAcidTempComp)
			}
		}
	}

	// Targets and limits
	if c.IinLimit_mA != nil {
		_ = d.dev.SetIinLimit_mA(*c.IinLimit_mA)
	}
	if c.IChargeTarget_mA != nil {
		if err := d.dev.SetIChargeTarget_mA(*c.IChargeTarget_mA); err != nil {
			if err == ltc4015.ErrTargetsReadOnly {
				_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "targets_read_only"})
				_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: "targets_read_only"})
			} else {
				d.errChg("set_charge_target_failed", err)
			}
		}
	}
	if c.IinHigh_mA != nil {
		if err := d.dev.SetIINHigh_mA(*c.IinHigh_mA); err == nil {
			d.desiredLimit |= ltc4015.IINHi
		} else {
			d.errChg("set_iin_high_failed", err)
		}
	}
	if c.IbatLow_mA != nil {
		if err := d.dev.SetIBATLow_mA(*c.IbatLow_mA); err == nil {
			d.desiredLimit |= ltc4015.IBATLo
		} else {
			d.errChg("set_ibat_low_failed", err)
		}
	}
	if c.DieTempHigh_mC != nil {
		if err := d.dev.SetDieTempHigh_mC(*c.DieTempHigh_mC); err == nil {
			d.desiredLimit |= ltc4015.DieTempHi
		} else {
			d.errChg("set_dietemp_high_failed", err)
		}
	}
	if c.BSRHigh_uOhmPerCell != nil {
		if err := d.dev.SetBSRHigh_uOhmPerCell(*c.BSRHigh_uOhmPerCell); err == nil {
			d.desiredLimit |= ltc4015.BSRHi
		} else {
			d.errChg("set_bsr_high_failed", err)
		}
	}

	// Windows and UVCL (record for opposite-edge re-arming)
	if c.VinLo_mV != nil || c.VinHi_mV != nil {
		lo, hi := deref[int32](c.VinLo_mV, 0), deref[int32](c.VinHi_mV, 0)
		if err := d.dev.SetVINWindowAndClear(lo, hi); err == nil {
			d.lastVinLo, d.lastVinHi = lo, hi
		} else {
			d.errChg("set_vin_window_failed", err)
		}
	}
	if c.VsysLo_mV != nil || c.VsysHi_mV != nil {
		lo, hi := deref[int32](c.VsysLo_mV, 0), deref[int32](c.VsysHi_mV, 0)
		if err := d.dev.SetVSYSWindowAndClear(lo, hi); err == nil {
			d.lastVsysLo, d.lastVsysHi = lo, hi
		} else {
			d.errChg("set_vsys_window_failed", err)
		}
	}
	if c.VbatLo_mVPerCell != nil || c.VbatHi_mVPerCell != nil {
		lo, hi := deref[int32](c.VbatLo_mVPerCell, 0), deref[int32](c.VbatHi_mVPerCell, 0)
		if err := d.dev.SetVBATWindowPerCellAndClear(lo, hi); err == nil {
			d.lastVbatLoCell, d.lastVbatHiCell = lo, hi
		} else {
			d.errChg("set_vbat_window_failed", err)
		}
	}
	if c.NTCRatioHi != nil || c.NTCRatioLo != nil {
		hi, lo := deref[uint16](c.NTCRatioHi, 0), deref[uint16](c.NTCRatioLo, 0)
		if err := d.dev.SetNTCRatioWindowAndClear(hi, lo); err == nil {
			d.lastNTCHi, d.lastNTCLo = hi, lo
		} else {
			d.errChg("set_ntc_ratio_window_failed", err)
		}
	}
	if c.VinUVCL_mV != nil {
		_ = d.dev.SetVinUvcl_mV(*c.VinUVCL_mV)
	}

	// User masks set desired sources (auto re-arming still applies).
	if m := c.AlertMask; m != nil {
		if m.Limit != nil {
			d.desiredLimit = ltc4015.LimitEnable(*m.Limit)
		}
		if m.ChgState != nil {
			d.desiredState = ltc4015.ChargerStateEnable(*m.ChgState)
		}
		if m.ChgStatus != nil {
			d.desiredStatus = ltc4015.ChargeStatusEnable(*m.ChgStatus)
		}
	}
}

// tiny generic pointer-deref helper
func deref[T any](p *T, zero T) T {
	if p != nil {
		return *p
	}
	return zero
}

// ---------- Alert service (ARA + drain + opposite-edge re-arming) ----------

func (d *Device) serviceAlertBatch() {
	const maxIters = 64
	it := 0

	// Ensure any pending retry is stopped before processing a fresh batch.
	if d.retryTimer != nil {
		if !d.retryTimer.Stop() {
			select {
			case <-d.retryTimer.C:
			default:
			}
		}
	}

	// Best-effort: where IRQ is unavailable, callers may still call this to poll.
	for d.dev.AlertActive(func() bool { return d.gpio.Get() }) && it < maxIters {
		it++

		// Try the full SMBus ARA path first; if some other device responded or error,
		// fall back to direct drain (which also clears).
		var ev ltc4015.AlertEvent
		if e, ok, err := d.dev.ServiceSMBAlert(); err == nil && ok {
			ev = e
		} else {
			// Not ours or error: attempt a direct drain to snapshot+clear our latches.
			if de, derr := d.dev.DrainAlerts(); derr == nil {
				ev = de
			}
		}
		if ev.Empty() {
			// No latches from us this iteration. Short back-off then re-check.
			time.Sleep(2 * time.Millisecond)
			continue
		}

		// Translate events to tags.
		if ev.Limit.Has(ltc4015.VINLo) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "vin_lo"})
		}
		if ev.Limit.Has(ltc4015.VINHi) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "vin_hi"})
		}
		if ev.Limit.Has(ltc4015.BSRHi) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, EventTag: "bsr_high"})
		}
		for _, t := range chgStateTags {
			if ev.ChgState.Has(t.bit) {
				_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: t.tag})
			}
		}
		for _, t := range chgStatusTags {
			if ev.ChgStatus.Has(t.bit) {
				_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: t.tag})
			}
		}

		// State-aware opposite-edge re-arming for ALL groups, then publish snapshot.
		d.rearm()
		d.sampleAndPublish()

		time.Sleep(2 * time.Millisecond)
	}

	// Still asserted? Arm a short retry. Otherwise, ensure the timer is disarmed.
	if d.alive.Load() && d.dev.AlertActive(func() bool { return d.gpio.Get() }) {
		if d.retryTimer == nil {
			d.retryTimer = time.NewTimer(2 * time.Millisecond)
		} else {
			d.retryTimer.Reset(2 * time.Millisecond)
		}
	} else if d.retryTimer != nil {
		if !d.retryTimer.Stop() {
			select {
			case <-d.retryTimer.C:
			default:
			}
		}
	}
}

var chgStateTags = []struct {
	bit ltc4015.ChargerStateBits
	tag string
}{
	{ltc4015.BatMissingFault, "bat_missing"},
	{ltc4015.BatShortFault, "bat_short"},
	{ltc4015.MaxChargeTimeFault, "max_charge_time_fault"},
	{ltc4015.AbsorbCharge, "absorb"},
	{ltc4015.EqualizeCharge, "equalize"},
	{ltc4015.CCCVCharge, "cccv"},
	{ltc4015.Precharge, "precharge"},
}

var chgStatusTags = []struct {
	bit ltc4015.ChargeStatusBits
	tag string
}{
	{ltc4015.IinLimitActive, "iin_limited"},
	{ltc4015.VinUvclActive, "uvcl_active"},
	{ltc4015.ConstCurrent, "cc_phase"},
	{ltc4015.ConstVoltage, "cv_phase"},
}

// ---- Re-arming (delegated to driver) ----
func (d *Device) rearm() {
	_ = d.dev.RearmOppositeEdges(
		ltc4015.DesiredMasks{
			Limit:     d.desiredLimit,
			ChgState:  d.desiredState,
			ChgStatus: d.desiredStatus,
		},
		ltc4015.Windows{
			VinLo_mV:      d.lastVinLo,
			VinHi_mV:      d.lastVinHi,
			VsysLo_mV:     d.lastVsysLo,
			VsysHi_mV:     d.lastVsysHi,
			VbatLoCell_mV: d.lastVbatLoCell,
			VbatHiCell_mV: d.lastVbatHiCell,
			NTCHi:         d.lastNTCHi,
			NTCLo:         d.lastNTCLo,
		},
	)
}

// ---- Telemetry ----

func (d *Device) sampleAndPublish() {
	ok, err := d.dev.MeasSystemValid()
	if err != nil {
		d.errBoth("meas_error", err)
		return
	}
	if !ok {
		_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: "meas_invalid"})
		_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: "meas_invalid"})
		return
	}

	// Use driver snapshot
	s := d.dev.Snapshot()

	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Payload: types.BatteryValue{
		PackMilliV:      s.Pack_mV,
		PerCellMilliV:   s.PerCell_mV,
		IBatMilliA:      s.IBat_mA,
		TempMilliC:      s.Die_mC,
		BSR_uOhmPerCell: s.BSR_uOhmPerCell,
	}})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Payload: types.ChargerValue{
		VIN_mV:  s.Vin_mV,
		VSYS_mV: s.Vsys_mV,
		IIn_mA:  s.IIn_mA,
		State:   uint16(s.State),
		Status:  uint16(s.Status),
		Sys:     uint16(s.System),
	}})

	// Temperature via NTC ratio (Beta equation)
	if ratio := s.NTCRatio; ratio != 0 {
		if deciC, ok := ntcRatioToDeciC(ratio, d.params.NTCBiasOhm, d.params.R25Ohm, d.params.BetaK); ok {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aTmp, Payload: types.TemperatureValue{DeciC: deciC}})
		} else {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aTmp, Err: "ntc_ratio_invalid"})
		}
	}
}

// ---- Errors ----

func (d *Device) errBoth(tag string, err error) {
	code := string(errcode.MapDriverErr(err))
	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, EventTag: tag})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: tag})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: code})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: code})
}
func (d *Device) errChg(tag string, err error) {
	code := string(errcode.MapDriverErr(err))
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: tag})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: code})
}

// Local helper: translate string param → driver constant.
func chemParamToExpect(s string) (ltc4015.ExpectChem, bool) {
	switch s {
	case "leadacid":
		return ltc4015.ExpectLeadAcid, true
	case "lifepo":
		return ltc4015.ExpectLiFePO4, true
	case "lithium":
		return ltc4015.ExpectLithiumIon, true
	default:
		return 0, false
	}
}

// ---- Maths ----

func ntcRatioToDeciC(ratio uint16, rbias, r25, beta uint32) (int16, bool) {
	if ratio == 0 || ratio >= 21845 || r25 == 0 || beta == 0 {
		return 0, false
	}
	Rntc := float64(rbias) * float64(ratio) / float64(21845-ratio)
	T := 1.0 / (1.0/298.15 + math.Log(Rntc/float64(r25))/float64(beta)) // kelvin
	Cd := math.Round((T - 273.15) * 10.0)                               // deci-°C
	return int16(Cd), true
}

// ---- Desired masks (defaults) ----

func (d *Device) desiredChargeStatusMask() ltc4015.ChargeStatusEnable {
	return ltc4015.IinLimitActive |
		ltc4015.VinUvclActive |
		ltc4015.ConstCurrent |
		ltc4015.ConstVoltage
}

func (d *Device) desiredChargerStateMask() ltc4015.ChargerStateEnable {
	return ltc4015.BatMissingFault |
		ltc4015.BatShortFault |
		ltc4015.MaxChargeTimeFault |
		ltc4015.AbsorbCharge |
		ltc4015.EqualizeCharge |
		ltc4015.CCCVCharge |
		ltc4015.Precharge
}

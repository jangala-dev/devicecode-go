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

// Device is a single-goroutine HAL device for LTC4015.
type Device struct {
	id   string
	aBat core.CapAddr // power/battery/<name>
	aChg core.CapAddr // power/charger/<name>
	aTmp core.CapAddr

	res   core.Resources
	i2c   drivers.I2C
	pin   int
	gpio  core.GPIOHandle
	es    core.GPIOEdgeStream
	alive atomic.Bool

	params Params

	// Owned by the worker only:
	dev *ltc4015.Device

	// Single-owner worker channels
	reqCh chan request
	// optional retry timer for asserted/held SMBALERT#
	retry *time.Timer
	done  chan struct{}
}

// Emit a tagged event and degraded status on both caps.
func (d *Device) evtErrBoth(tag, code string) {
	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, EventTag: tag})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: tag})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: code})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: code})
}

// Charger-only: tagged event + degraded.
func (d *Device) evtErrChg(tag, code string) {
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: tag})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: code})
}

// Battery-only: tagged event + degraded.
func (d *Device) evtErrBat(tag, code string) {
	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, EventTag: tag})
	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: code})
}

type opCode uint8

const (
	opSampleAll opCode = iota
	opReadCharger
	opEnableCharging
	opSetIinLimit
	opSetIChargeTarget
	opSetVinWindow
	opStop
)

type request struct {
	op  opCode
	arg any
}

// ---- core.Device interface ----

func (d *Device) ID() string { return d.id }

func (d *Device) Capabilities() []core.CapabilitySpec {
	// Static info payloads (retained by HAL at registration).
	bi := types.BatteryInfo{
		Cells:      d.params.Cells,
		Chem:       d.params.Chem,
		RSNSB_uOhm: d.params.RSNSB_uOhm,
		Bus:        d.params.Bus,
		Addr:       addrOrDefault(d.params.Addr),
	}
	ci := types.ChargerInfo{
		RSNSI_uOhm: d.params.RSNSI_uOhm,
		Bus:        d.params.Bus,
		Addr:       addrOrDefault(d.params.Addr),
	}

	return []core.CapabilitySpec{
		{
			Domain: d.aBat.Domain,
			Kind:   types.KindBattery,
			Name:   d.aBat.Name,
			Info:   types.Info{SchemaVersion: 1, Driver: "ltc4015", Detail: bi},
		},
		{
			Domain: d.aChg.Domain,
			Kind:   types.KindCharger,
			Name:   d.aChg.Name,
			Info:   types.Info{SchemaVersion: 1, Driver: "ltc4015", Detail: ci},
		},
		{
			Domain: d.aTmp.Domain, Kind: types.KindTemperature, Name: d.aTmp.Name,
			Info: types.Info{
				SchemaVersion: 1, Driver: "ltc4015", Detail: types.TemperatureInfo{
					Sensor: "ntc@ltc4015", Addr: addrOrDefault(d.params.Addr), Bus: d.params.Bus,
				},
			},
		},
	}
}

func (d *Device) Init(ctx context.Context) error {
	// Subscribe to falling edges on SMBALERT# with a modest buffer.
	es, err := d.res.Reg.SubscribeGPIOEdges(d.id, d.pin, core.EdgeFalling, 2*time.Millisecond, 8)
	if err != nil {
		// Publish degraded status upfront; continue with worker regardless.
		d.evtErrBoth("alert_subscribe_failed", "alert_subscribe_failed")
		// We still start the worker; SMBALERT# will be polled via line level checks.
	}
	d.es = es

	// Set up worker channels and start the single worker goroutine.
	d.reqCh = make(chan request, 8)
	d.retry = nil
	d.done = make(chan struct{})

	d.alive.Store(true)
	go d.worker(ctx)
	return nil
}

func (d *Device) Close() error {
	if d.alive.Load() {
		// best-effort stop
		select {
		case d.reqCh <- request{op: opStop}:
			d.alive.Store(false)
		default:
		}
		// bounded wait; rely on HAL ctx cancellation in normal shutdown
		t := time.NewTimer(300 * time.Millisecond)
		select {
		case <-d.done:
		case <-t.C:
			// Worker did not exit in time: force a local cleanup so that registry claims
			// are not left held. This matches HAL’s expectation that Close() releases claims.
			d.cleanup()
		}
		t.Stop()
	}
	return nil
}

func (d *Device) Control(_ core.CapAddr, verb string, payload any) (core.EnqueueResult, error) {
	// Map verbs to requests; all controls are non-blocking enqueue-only.
	send := func(req request) (core.EnqueueResult, error) {
		if !d.alive.Load() {
			return core.EnqueueResult{OK: false, Error: errcode.Unavailable}, nil
		}
		select {
		case d.reqCh <- req:
			return core.EnqueueResult{OK: true}, nil
		default:
			return core.EnqueueResult{OK: false, Error: errcode.Busy}, nil
		}
	}

	switch verb {
	case "read":
		// Non-blocking enqueue; HAL guarantees retained values for consumers.
		// Polling should be used for periodic sampling.
		return send(request{op: opSampleAll})
	case "enable":
		v, code := core.As[types.ChargerEnable](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		return send(request{op: opEnableCharging, arg: v})

	case "set_input_limit":
		v, code := core.As[types.SetInputLimit](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		return send(request{op: opSetIinLimit, arg: v})

	case "set_charge_target":
		v, code := core.As[types.SetChargeTarget](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		return send(request{op: opSetIChargeTarget, arg: v})

	case "set_vin_window":
		v, code := core.As[types.SetVinWindow](payload)
		if code != "" {
			return core.EnqueueResult{OK: false, Error: code}, nil
		}
		return send(request{op: opSetVinWindow, arg: v})

	default:
		return core.EnqueueResult{OK: false, Error: errcode.Unsupported}, nil
	}
}

// ---- Worker ----

func (d *Device) worker(ctx context.Context) {
	defer func() { d.alive.Store(false) }()

	// Construct driver and do all configuration here (single owner).
	d.configureDevice()

	// If line already asserted, service once.
	if d.dev.AlertActive(func() bool { return d.gpio.Get() }) {
		d.serviceSMBAlertWhileLow()
	}

	// Event channel from edge subscription (if present).
	var evCh <-chan core.GPIOEdgeEvent
	if d.es != nil {
		evCh = d.es.Events()
	}

	liveness := time.NewTicker(100 * time.Millisecond)
	defer liveness.Stop()

	defer close(d.done)

	for {
		select {
		case <-ctx.Done():
			d.cleanup()
			return

		case req := <-d.reqCh:
			switch req.op {
			case opSampleAll, opReadCharger:
				d.sampleAndPublish()
			case opEnableCharging:
				if v, ok := req.arg.(types.ChargerEnable); ok {
					d.setEnable(v.On)
				}
			case opSetIinLimit:
				if v, ok := req.arg.(types.SetInputLimit); ok {
					d.setIinLimit(v.MilliA)
				}
			case opSetIChargeTarget:
				if v, ok := req.arg.(types.SetChargeTarget); ok {
					d.setIChargeTarget(v.MilliA)
				}
			case opSetVinWindow:
				if v, ok := req.arg.(types.SetVinWindow); ok {
					d.setVinWindow(v.Lo_mV, v.Hi_mV)
				}
			case opStop:
				d.cleanup()
				return
			}

		case _, ok := <-evCh:
			if !ok {
				evCh = nil
				break
			}
			d.serviceSMBAlertWhileLow()

		case <-d.retryC():
			// Timer fired to revisit a still-asserted ALERT#.
			d.serviceSMBAlertWhileLow()

		case <-liveness.C: // THIS IS NOT A PERFECT CHECK FOR LIVENESS - IT WILL FAIL IF WE PURPOSEFULLY SET MEAS SYS OFF
			if ok, err := d.dev.MeasSystemValid(); err == nil && !ok {
				d.configureDevice()
				// Queue a sample to reseed retained values.
				select {
				case d.reqCh <- request{op: opSampleAll}:
				default:
				}
			}
		}
	}
}

func (d *Device) retryC() <-chan time.Time {
	if d.retry == nil {
		return nil // nil channel blocks forever in select
	}
	return d.retry.C
}

// ---- Worker helpers (single-owner context) ----

func (d *Device) configureDevice() {
	cfg := ltc4015.Config{
		Address:         addrOrDefault(d.params.Addr),
		RSNSB_uOhm:      d.params.RSNSB_uOhm,
		RSNSI_uOhm:      d.params.RSNSI_uOhm,
		Cells:           d.params.Cells,
		QCountPrescale:  d.params.QCountPrescale,
		TargetsWritable: d.params.TargetsWritable,
	}

	// Chemistry selection
	switch d.params.Chem {
	case "leadacid":
		cfg.Chem = ltc4015.ChemLeadAcid
	case "auto", "detect":
		cfg.Chem = ltc4015.ChemUnknown
	default:
		cfg.Chem = ltc4015.ChemLithium
	}

	drv := ltc4015.New(d.i2c, cfg)
	if cfg.Chem == ltc4015.ChemUnknown {
		if c, err := drv.DetectChemistry(); err == nil && c != ltc4015.ChemUnknown {
			// Fix chemistry once detected.
			// (It is stored inside the driver; no need to rewrite cfg.)
		} else if err != nil {
			d.evtErrBoth("chem_detect_failed", string(errcode.MapDriverErr(err)))
		}
	}
	if err := drv.Configure(cfg); err != nil {
		d.evtErrBoth("configure_failed", string(errcode.MapDriverErr(err)))
	}

	// Keep telemetry running; enable coulomb counter.
	if err := drv.SetConfigBits(ltc4015.ForceMeasSysOn | ltc4015.EnableQCount); err != nil {
		d.evtErrBoth("set_config_bits_failed", string(errcode.MapDriverErr(err)))
	}

	// Optional thresholds.
	if d.params.VinLo_mV != 0 || d.params.VinHi_mV != 0 {
		if err := drv.SetVINWindow_mV(d.params.VinLo_mV, d.params.VinHi_mV); err != nil {
			d.evtErrChg("set_vin_window_failed", string(errcode.MapDriverErr(err)))
		}
	}
	if d.params.BSRHi_uOhmPerCell != 0 {
		if err := drv.SetBSRHigh_uOhmPerCell(d.params.BSRHi_uOhmPerCell); err != nil {
			d.evtErrBat("set_bsr_high_failed", string(errcode.MapDriverErr(err)))
		}
	}

	// Enable key alerts and clear latches.
	if err := drv.EnableChargerStateAlertsMask(ltc4015.BatMissingFault | ltc4015.BatShortFault | ltc4015.MaxChargeTimeFault); err != nil {
		d.evtErrChg("enable_chg_state_alerts_failed", string(errcode.MapDriverErr(err)))
	}
	if err := drv.ClearChargerStateAlerts(); err != nil {
		d.evtErrChg("clear_chg_state_alerts_failed", string(errcode.MapDriverErr(err)))
	}

	// CHARGE_STATUS edge-driven: enable only edges that are not currently asserted.
	en := baseStatusMask()
	if cs, err := drv.ChargeStatus(); err == nil {
		en &^= cs
	}
	if err := drv.EnableChargeStatusAlertsMask(en); err != nil {
		d.evtErrChg("enable_charge_status_alerts_failed", string(errcode.MapDriverErr(err)))
	}
	if err := drv.ClearChargeStatusAlerts(); err != nil {
		d.evtErrChg("clear_charge_status_alerts_failed", string(errcode.MapDriverErr(err)))
	}

	// VIN edge-driven mask (persistently include BSRHi).
	d.initVinMask(drv)

	d.dev = drv

	// Enqueue initial sample to seed retained values
	select {
	case d.reqCh <- request{op: opSampleAll}:
	default:
		// ignore if queue temporarily full
	}
}

func (d *Device) initVinMask(dev *ltc4015.Device) {
	const defaultHi = 11000 // fallback if no thresholds provided
	const defaultLo = 9000
	lo := d.params.VinLo_mV
	hi := d.params.VinHi_mV
	if lo == 0 && hi == 0 {
		lo, hi = defaultLo, defaultHi
	}
	if err := dev.SetVINWindow_mV(lo, hi); err != nil {
		d.evtErrChg("set_vin_window_failed", string(errcode.MapDriverErr(err)))
		// Continue with conservative mask
		d.setVinEdgeMask(dev, ltc4015.VINLo|ltc4015.VINHi)
		return
	}

	mv, err := dev.VinMilliV()
	if err != nil {
		d.setVinEdgeMask(dev, ltc4015.VINLo|ltc4015.VINHi)
		return
	}
	switch {
	case mv >= hi:
		d.setVinEdgeMask(dev, ltc4015.VINLo)
	case mv <= lo:
		d.setVinEdgeMask(dev, ltc4015.VINHi)
	default:
		d.setVinEdgeMask(dev, ltc4015.VINLo|ltc4015.VINHi)
	}
}

func (d *Device) setVinEdgeMask(dev *ltc4015.Device, mask ltc4015.LimitEnable) {
	mask |= ltc4015.BSRHi // always keep BSR high enabled
	if err := dev.EnableLimitAlertsMask(mask); err != nil {
		d.evtErrChg("enable_limit_alerts_failed", string(errcode.MapDriverErr(err)))
	}
	if err := dev.ClearLimitAlerts(); err != nil {
		d.evtErrChg("clear_limit_alerts_failed", string(errcode.MapDriverErr(err)))
	}
}

func (d *Device) sampleAndPublish() {
	// Only publish when measurements are valid; otherwise drive degraded status.
	ok, err := d.dev.MeasSystemValid()
	if err != nil {
		code := string(errcode.MapDriverErr(err))
		_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: code})
		_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: code})
		return
	}
	if !ok {
		_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: "meas_invalid"})
		_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: "meas_invalid"})
		return
	}

	// Battery group
	var bv types.BatteryValue
	if pv, err := d.dev.BatteryMilliVPack(); err == nil {
		bv.PackMilliV = pv
	}
	if cv, err := d.dev.BatteryMilliVPerCell(); err == nil {
		bv.PerCellMilliV = cv
	}
	if ib, err := d.dev.IbatMilliA(); err == nil {
		bv.IBatMilliA = ib
	}
	if mc, err := d.dev.DieMilliC(); err == nil {
		bv.TempMilliC = mc
	}
	if bsr, err := d.dev.BSRMicroOhmPerCell(); err == nil {
		bv.BSR_uOhmPerCell = bsr
	}
	_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Payload: bv})

	// Charger group
	var cv types.ChargerValue
	if vin, err := d.dev.VinMilliV(); err == nil {
		cv.VIN_mV = vin
	}
	if vsys, err := d.dev.VsysMilliV(); err == nil {
		cv.VSYS_mV = vsys
	}
	if iin, err := d.dev.IinMilliA(); err == nil {
		cv.IIn_mA = iin
	}
	if st, err := d.dev.ChargerState(); err == nil {
		cv.State = uint16(st)
	}
	if cs, err := d.dev.ChargeStatus(); err == nil {
		cv.Status = uint16(cs)
	}
	if ss, err := d.dev.SystemStatus(); err == nil {
		cv.Sys = uint16(ss)
	}
	_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Payload: cv})

	if ratio, err := d.dev.NTCRatio(); err != nil {
		_ = d.res.Pub.Emit(core.Event{Addr: d.aTmp, Err: string(errcode.MapDriverErr(err))})
	} else if deciC, ok := ntcRatioToDeciC(ratio, d.params.NTCBiasOhm, d.params.R25Ohm, d.params.BetaK); ok {
		_ = d.res.Pub.Emit(core.Event{
			Addr: d.aTmp, Payload: types.TemperatureValue{DeciC: deciC},
		})
	} else {
		_ = d.res.Pub.Emit(core.Event{Addr: d.aTmp, Err: "ntc_ratio_invalid"})
	}
}

func (d *Device) serviceSMBAlertWhileLow() {
	const maxIters = 64
	iters := 0

	// Ensure any pending retry is stopped; we'll reschedule if still asserted later.
	if d.retry != nil {
		if !d.retry.Stop() {
			select {
			case <-d.retry.C:
			default:
			}
		}
	}

	for !d.gpio.Get() && iters < maxIters {
		iters++

		ev, ok, err := d.dev.ServiceSMBAlert()
		if err != nil {
			// Publish degraded statuses and back-off.
			_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, Err: string(errcode.MapDriverErr(err))})
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, Err: string(errcode.MapDriverErr(err))})
			// back-off: 2ms << min(iters, 4) → max 32ms
			time.Sleep(time.Duration(1<<min(iters, 4)) * time.Millisecond)
			continue
		}
		if !ok {
			// ARA reported another device or no responder; if line still low, retry soon.
			time.Sleep(2 * time.Millisecond)
			continue
		}

		// Translate events.
		d.translateAlerts(ev)

		// Brief pause between drains to allow latches to settle.
		time.Sleep(2 * time.Millisecond)
	}

	// Still asserted after our cap or after servicing? Schedule a retry.
	if !d.gpio.Get() {
		if d.retry == nil {
			d.retry = time.NewTimer(2 * time.Millisecond)
		} else {
			d.retry.Reset(2 * time.Millisecond)
		}
	}
}

func (d *Device) translateAlerts(ev ltc4015.AlertEvent) {

	// ---- Limit alerts: VIN window and BSR ----
	if vinBits := ev.Limit & (ltc4015.VINLo | ltc4015.VINHi); vinBits != 0 {
		// Re-resolve state and re-arm opposite edge.
		mv, _ := d.dev.VinMilliV()
		lo := d.params.VinLo_mV
		hi := d.params.VinHi_mV
		if lo == 0 && hi == 0 {
			lo, hi = 9000, 11000
		}
		switch {
		case mv >= hi:
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "vin_connected"})
			d.setVinEdgeMask(d.dev, ltc4015.VINLo)
		case mv <= lo:
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "vin_disconnected"})
			d.setVinEdgeMask(d.dev, ltc4015.VINHi)
		default:
			d.setVinEdgeMask(d.dev, ltc4015.VINLo|ltc4015.VINHi)
		}
	}
	if ev.Limit.Has(ltc4015.BSRHi) {
		_ = d.res.Pub.Emit(core.Event{Addr: d.aBat, EventTag: "bsr_high"})
	}

	// ---- Charger state: faults and phase edges (events), then re-arm + clear ----
	if ev.ChgState != 0 {
		s := ev.ChgState
		// Emit a tag for each asserted bit we care about.
		if s.Has(ltc4015.BatMissingFault) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "bat_missing"})
		}
		if s.Has(ltc4015.BatShortFault) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "bat_short"})
		}
		if s.Has(ltc4015.MaxChargeTimeFault) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "max_charge_time_fault"})
		}
		if s.Has(ltc4015.AbsorbCharge) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "absorb"})
		}
		if s.Has(ltc4015.EqualizeCharge) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "equalize"})
		}
		if s.Has(ltc4015.CCCVCharge) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "cccv"})
		}
		if s.Has(ltc4015.Precharge) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "precharge"})
		}

		// Re-arm charger-state edges: enable only bits that are NOT currently asserted.
		// Include both faults and phases you want edge notifications for.
		en := ltc4015.BatMissingFault |
			ltc4015.BatShortFault |
			ltc4015.MaxChargeTimeFault |
			ltc4015.AbsorbCharge |
			ltc4015.EqualizeCharge |
			ltc4015.CCCVCharge |
			ltc4015.Precharge

		if cur, err := d.dev.ChargerState(); err == nil {
			en &^= cur
			if err := d.dev.EnableChargerStateAlertsMask(en); err != nil {
				d.evtErrChg("enable_chg_state_alerts_failed", string(errcode.MapDriverErr(err)))
			}
			if err := d.dev.ClearChargerStateAlerts(); err != nil {
				d.evtErrChg("clear_chg_state_alerts_failed", string(errcode.MapDriverErr(err)))
			}
		} else {
			d.evtErrChg("read_charger_state_failed", string(errcode.MapDriverErr(err)))
		}
	}

	// ---- Charge status edges: events, then re-arm + clear ----
	if ev.ChgStatus != 0 {
		s := ev.ChgStatus
		if s.Has(ltc4015.IinLimitActive) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "iin_limited"})
		}
		if s.Has(ltc4015.VinUvclActive) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "uvcl_active"})
		}
		if s.Has(ltc4015.ConstCurrent) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "cc_phase"})
		}
		if s.Has(ltc4015.ConstVoltage) {
			_ = d.res.Pub.Emit(core.Event{Addr: d.aChg, EventTag: "cv_phase"})
		}

		en := baseStatusMask()
		if cur, err := d.dev.ChargeStatus(); err == nil {
			en &^= cur
			if err := d.dev.EnableChargeStatusAlertsMask(en); err != nil {
				d.evtErrChg("enable_charge_status_alerts_failed", string(errcode.MapDriverErr(err)))
			}
			if err := d.dev.ClearChargeStatusAlerts(); err != nil {
				d.evtErrChg("clear_charge_status_alerts_failed", string(errcode.MapDriverErr(err)))
			}
		} else {
			d.evtErrChg("read_charge_status_failed", string(errcode.MapDriverErr(err)))
		}
	}

	// ---- Snapshot after handling a batch ----
	d.sampleAndPublish()
}

func (d *Device) setEnable(on bool) {
	var err error
	if on {
		err = d.dev.ClearConfigBits(ltc4015.SuspendCharger)
	} else {
		err = d.dev.SetConfigBits(ltc4015.SuspendCharger)
	}
	if err != nil {
		d.evtErrChg("set_enable_failed", string(errcode.MapDriverErr(err)))
		return
	}
	// Publish a fresh snapshot.
	d.sampleAndPublish()
}

func (d *Device) setIinLimit(mA int32) {
	if err := d.dev.SetIinLimit_mA(mA); err != nil {
		d.evtErrChg("set_iin_limit_failed", string(errcode.MapDriverErr(err)))
		return
	}
	d.sampleAndPublish()
}

func (d *Device) setIChargeTarget(mA int32) {
	if !d.params.TargetsWritable {
		// Reflect unsupported as degraded event.
		d.evtErrChg("targets_read_only", "targets_read_only")
		return
	}
	if err := d.dev.SetIChargeTarget_mA(mA); err != nil {
		d.evtErrChg("set_charge_target_failed", string(errcode.MapDriverErr(err)))
		return
	}
	d.sampleAndPublish()
}

func (d *Device) setVinWindow(lo, hi int32) {
	if err := d.dev.SetVINWindow_mV(lo, hi); err != nil {
		d.evtErrChg("set_vin_window_failed", string(errcode.MapDriverErr(err)))
		return
	}
	// Recompute edge-arming after changing thresholds.
	d.initVinMask(d.dev)
}

func (d *Device) cleanup() {
	// Stop retry timer if any.
	if d.retry != nil {
		if !d.retry.Stop() {
			select {
			case <-d.retry.C:
			default:
			}
		}
	}
	// Close edge stream and release claims from the worker side as well.
	if d.es != nil {
		d.es.Close()
		d.res.Reg.UnsubscribeGPIOEdges(d.id, d.pin)
		d.es = nil
	}
	d.res.Reg.ReleasePin(d.id, d.pin)
	d.res.Reg.ReleaseI2C(d.id, core.ResourceID(d.params.Bus))
}

// ratio(1..21844) -> deci-°C using the Beta equation.
func ntcRatioToDeciC(ratio uint16, rbias, r25, beta uint32) (int16, bool) {
	if ratio == 0 || ratio >= 21845 || r25 == 0 || beta == 0 {
		return 0, false
	}

	Rntc := float64(rbias) * float64(ratio) / float64(21845-ratio)
	T := 1.0 / (1.0/298.15 + math.Log(Rntc/float64(r25))/float64(beta)) // kelvin
	Cd := math.Round((T - 273.15) * 10.0)                               // deci-°C
	return int16(Cd), true
}

// ---- Helpers ----

func baseStatusMask() ltc4015.ChargeStatusEnable {
	return ltc4015.VinUvclActive |
		ltc4015.IinLimitActive |
		ltc4015.ConstCurrent |
		ltc4015.ConstVoltage
}

func addrOrDefault(a uint16) uint16 {
	if a == 0 {
		return ltc4015.AddressDefault
	}
	return a
}

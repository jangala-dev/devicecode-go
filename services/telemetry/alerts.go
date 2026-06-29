package telemetry

import "devicecode-go/types"

// chargerAlertFSM holds previous bitfield state; on bit-set transition
// for a kind, it emits one normal event with the canonical kind name.
// The 14 canonical kinds split into:
//   - 11 bit-driven kinds (state[] + status[]), compared against the
//     previous ChargerValue snapshot
//   - 3 analog kinds (vin_lo / vin_hi / bsr_high), compared against
//     the thresholds carried on state/self/power/charger/config.
//     vin_lo + vin_hi observe ChargerValue.VIN_mV; bsr_high observes
//     BatteryValue.BSR_uOhmPerCell.
//
// Each kind fires only on the boundary-crossing edge. While a value
// stays past its threshold (or a bit stays set), no further alerts.
type chargerAlertFSM struct {
	prev    types.ChargerValue
	prevBSR uint32
	seen    bool
	seenBSR bool
}

// AlertKind is the canonical alert kind name (snake_case) sent on
// the wire as event/self/power/charger/alert.kind. The 14 values
// are frozen by the spec; new kinds must be added here AND on the
// CM5 import side.
type AlertKind string

const (
	AlertVinLo              AlertKind = "vin_lo"
	AlertVinHi              AlertKind = "vin_hi"
	AlertBsrHigh            AlertKind = "bsr_high"
	AlertBatMissing         AlertKind = "bat_missing"
	AlertBatShort           AlertKind = "bat_short"
	AlertMaxChargeTimeFault AlertKind = "max_charge_time_fault"
	AlertAbsorb             AlertKind = "absorb"
	AlertEqualize           AlertKind = "equalize"
	AlertCccv               AlertKind = "cccv"
	AlertPrecharge          AlertKind = "precharge"
	AlertIinLimited         AlertKind = "iin_limited"
	AlertUvclActive         AlertKind = "uvcl_active"
	AlertCcPhase            AlertKind = "cc_phase"
	AlertCvPhase            AlertKind = "cv_phase"
)

// AllAlertKinds enumerates every canonical kind. Tests assert this is
// exactly 14 entries and that publishing rejects anything outside the
// set.
var AllAlertKinds = []AlertKind{
	AlertVinLo, AlertVinHi, AlertBsrHigh,
	AlertBatMissing, AlertBatShort, AlertMaxChargeTimeFault,
	AlertAbsorb, AlertEqualize, AlertCccv, AlertPrecharge,
	AlertIinLimited, AlertUvclActive, AlertCcPhase, AlertCvPhase,
}

// AlertEvent is the payload at event/self/power/charger/alert. Not
// retained — the publisher uses retained=false so subscribers only
// see live transitions, not stale alerts on reconnect.
type AlertEvent struct {
	Kind       AlertKind `json:"kind"`
	Severity   string    `json:"severity"`
	Source     string    `json:"source"`
	StateBits  uint16    `json:"state_bits"`
	StatusBits uint16    `json:"status_bits"`
	SystemBits uint16    `json:"system_bits"`
	Seq        uint32    `json:"seq"`
	UptimeMs   int64     `json:"uptime_ms"`
}

// alertSeverity returns the canonical severity for a kind. Faults
// surface as "warning"; charge-phase / control-loop transitions are
// "info". Splitting it out keeps the FSM's emit loop tiny and gives
// the spec one place to grow if severity refines later.
func alertSeverity(k AlertKind) string {
	switch k {
	case AlertBatMissing, AlertBatShort, AlertMaxChargeTimeFault, AlertVinLo, AlertVinHi, AlertBsrHigh:
		return "warning"
	default:
		return "info"
	}
}

// observe runs one tick of the FSM against an incoming ChargerValue.
// On every bit-set transition we emit one event. Cleared bits do
// nothing (sparse stream — no clear-events).
func (f *chargerAlertFSM) observe(s *Service, v types.ChargerValue) {
	if !f.seen {
		f.prev = v
		f.seen = true
		return
	}

	// State bits (CHARGER_STATE_ALERTS): 6 of the 11 bits map to
	// canonical kinds. Bits with display name "suspended", "ntc_pause",
	// "timer_term", "c_over_x_term" don't map to alert kinds in the
	// spec — they're informational only.
	f.fireOnSet(s, v, uint16(types.BatMissingFault), AlertBatMissing,
		uint16(v.State), uint16(f.prev.State))
	f.fireOnSet(s, v, uint16(types.BatShortFault), AlertBatShort,
		uint16(v.State), uint16(f.prev.State))
	f.fireOnSet(s, v, uint16(types.MaxChargeTimeFault), AlertMaxChargeTimeFault,
		uint16(v.State), uint16(f.prev.State))
	f.fireOnSet(s, v, uint16(types.AbsorbCharge), AlertAbsorb,
		uint16(v.State), uint16(f.prev.State))
	f.fireOnSet(s, v, uint16(types.EqualizeCharge), AlertEqualize,
		uint16(v.State), uint16(f.prev.State))
	f.fireOnSet(s, v, uint16(types.CCCVCharge), AlertCccv,
		uint16(v.State), uint16(f.prev.State))
	f.fireOnSet(s, v, uint16(types.Precharge), AlertPrecharge,
		uint16(v.State), uint16(f.prev.State))

	// Status bits (CHARGE_STATUS): all 4 map to kinds.
	f.fireOnSet(s, v, uint16(types.IinLimitActive), AlertIinLimited,
		uint16(v.Status), uint16(f.prev.Status))
	f.fireOnSet(s, v, uint16(types.VinUvclActive), AlertUvclActive,
		uint16(v.Status), uint16(f.prev.Status))
	f.fireOnSet(s, v, uint16(types.ConstCurrent), AlertCcPhase,
		uint16(v.Status), uint16(f.prev.Status))
	f.fireOnSet(s, v, uint16(types.ConstVoltage), AlertCvPhase,
		uint16(v.Status), uint16(f.prev.Status))

	// Analog kinds — vin_lo and vin_hi compare ChargerValue.VIN_mV
	// against the published thresholds on state/self/power/charger/
	// config. bsr_high comes from BatteryValue and is handled in
	// observeBattery below.
	th := s.chargerThresholds()
	if th.VinLoMV > 0 {
		// Edge from "vin >= threshold" to "vin < threshold".
		if f.prev.VIN_mV >= th.VinLoMV && v.VIN_mV < th.VinLoMV {
			s.emitAlert(v, AlertVinLo)
		}
	}
	if th.VinHiMV > 0 {
		// Edge from "vin <= threshold" to "vin > threshold".
		if f.prev.VIN_mV <= th.VinHiMV && v.VIN_mV > th.VinHiMV {
			s.emitAlert(v, AlertVinHi)
		}
	}

	f.prev = v
}

// observeBattery feeds the bsr_high analog kind. BSR
// (battery-source-resistance) lives on BatteryValue, not
// ChargerValue, so it gets its own observer entry point.
func (f *chargerAlertFSM) observeBattery(s *Service, b types.BatteryValue) {
	if !f.seenBSR {
		f.prevBSR = b.BSR_uOhmPerCell
		f.seenBSR = true
		return
	}
	th := s.chargerThresholds()
	if th.BSRHighUohmPerCell > 0 {
		if f.prevBSR <= th.BSRHighUohmPerCell && b.BSR_uOhmPerCell > th.BSRHighUohmPerCell {
			// bsr_high carries the latest charger snapshot for context;
			// state/status/system bits are the most recent we saw.
			s.emitAlert(f.prev, AlertBsrHigh)
		}
	}
	f.prevBSR = b.BSR_uOhmPerCell
}

// fireOnSet emits an alert if the bit went from clear to set between
// prev and curr. Bit is passed as a uint16 mask — call sites convert
// from their typed bit-flag (types.ChargerStateBits etc.) at the
// callsite to keep this helper free of generics overhead.
func (f *chargerAlertFSM) fireOnSet(
	s *Service,
	v types.ChargerValue,
	mask uint16,
	kind AlertKind,
	curr, prev uint16,
) {
	if mask == 0 {
		return
	}
	wasSet := (prev & mask) != 0
	isSet := (curr & mask) != 0
	if !wasSet && isSet {
		s.emitAlert(v, kind)
	}
}

func (s *Service) emitAlert(v types.ChargerValue, kind AlertKind) {
	ev := AlertEvent{
		Kind:       kind,
		Severity:   alertSeverity(kind),
		Source:     "ltc4015",
		StateBits:  v.State,
		StatusBits: v.Status,
		SystemBits: v.Sys,
		Seq:        s.seqChargerAlert.Add(1),
		UptimeMs:   s.uptimeMs(),
	}
	// Sparse alerts: NOT retained.
	s.conn.PublishValue(TopicChargerAlert, ev, false)
}

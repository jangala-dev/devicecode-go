// Package telemetry implements the retained-state and sparse-alert
// publishers from ../docs/updating.md. It
// subscribes to the existing HAL value topics (hal/cap/env/...,
// hal/cap/power/...) and republishes them under the canonical
// state/self/* surface using integer engineering units, plus runs the
// charger alert FSM that emits event/self/power/charger/alert with
// 14 canonical kinds.
//
// Boundary: telemetry does NOT touch the updater state machine — it
// only consumes HAL data and produces fact retains + alert events.
// The fabric service then exports them onto the wire via the
// state/self/* + event/self/* export rules in services/fabric/remap.go.
package telemetry

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/retainedpub"
	"devicecode-go/types"
)

// Topic constants mirror the canonical fact schema in ../docs/updating.md.
var (
	TopicBattery     = bus.T("state", "self", "power", "battery")
	TopicCharger     = bus.T("state", "self", "power", "charger")
	TopicChargerCfg  = bus.T("state", "self", "power", "charger", "config")
	TopicEnvTemp     = bus.T("state", "self", "environment", "temperature")
	TopicEnvHumidity = bus.T("state", "self", "environment", "humidity")
	TopicRuntimeMem  = bus.T("state", "self", "runtime", "memory")

	TopicChargerAlert = bus.T("event", "self", "power", "charger", "alert")
)

// HAL source topics — single point of truth for what we subscribe to.
var (
	halEnvTemp = bus.T("hal", "cap", "env", string(types.KindTemperature), "core", "value")
	halEnvHum  = bus.T("hal", "cap", "env", string(types.KindHumidity), "core", "value")
	halPwrAny  = bus.T("hal", "cap", "power", "+", "internal", "value")
)

// MemSnapshotInterval is how often the runtime/memory fact republishes.
// Memory pressure is a diagnostic signal, not a safety input, so keep it
// much slower than HAL sampling to avoid burning UART bandwidth.
const MemSnapshotInterval = 30 * time.Second

// ChargerThresholds carries the analog comparison thresholds used by
// both the state/self/power/charger/config retained fact and the charger
// alert FSM's analog kinds: vin_lo, vin_hi, and bsr_high.
//
// These ARE the LTC4015 effective config in production; on this
// branch they default to conservative bring-up values.
type ChargerThresholds struct {
	VinLoMV            int32  `json:"vin_lo_mV"`
	VinHiMV            int32  `json:"vin_hi_mV"`
	BSRHighUohmPerCell uint32 `json:"bsr_high_uohm_per_cell"`
}

// ChargerAlertMask is the 14-bool mask matching the 14 canonical alert
// kinds. The mask is informational until the LTC4015 driver programs the
// chip's alert-enable register from this and reports it back; after that,
// masking can flow through to the FSM. Names here are wire-stable.
type ChargerAlertMask struct {
	VinLo              bool `json:"vin_lo"`
	VinHi              bool `json:"vin_hi"`
	BSRHigh            bool `json:"bsr_high"`
	BatMissing         bool `json:"bat_missing"`
	BatShort           bool `json:"bat_short"`
	MaxChargeTimeFault bool `json:"max_charge_time_fault"`
	Absorb             bool `json:"absorb"`
	Equalize           bool `json:"equalize"`
	CCCV               bool `json:"cccv"`
	Precharge          bool `json:"precharge"`
	IinLimited         bool `json:"iin_limited"`
	UvclActive         bool `json:"uvcl_active"`
	CcPhase            bool `json:"cc_phase"`
	CvPhase            bool `json:"cv_phase"`
}

// ChargerConfig is the typed input into the publisher; the runtime
// fact wraps it inside ChargerConfigFact with seq + uptime_ms.
//
// Source is the value emitted on the wire as the fact's "source"
// field. Use "ltc4015" when the driver has read the effective
// programmed register state; use "ltc4015-default" (the
// DefaultChargerConfig value) to make it visible on the wire that
// these are fallback bring-up values, not what the chip is actually
// programmed with. The source string tracks the data's provenance so defaults
// are not presented as values read back from the chip.
type ChargerConfig struct {
	Source        string
	Thresholds    ChargerThresholds
	AlertMaskBits uint16
	AlertMask     ChargerAlertMask
}

// DefaultChargerConfig returns conservative bring-up values labelled
// source="ltc4015-default" so consumers can spot that the LTC4015
// driver has not supplied effective programmed values. VinLoMV /
// VinHiMV bracket a healthy USB-C / 12 V input; BSRHigh targets a
// typical lead-acid pack BSR.
func DefaultChargerConfig() ChargerConfig {
	return ChargerConfig{
		Source: "ltc4015-default",
		Thresholds: ChargerThresholds{
			VinLoMV:            10500,
			VinHiMV:            17000,
			BSRHighUohmPerCell: 5000,
		},
		// Mask bits + booleans both zero — alerts unmasked at the
		// chip level by default. The FSM emits regardless on this
		// branch (informational mask only).
	}
}

// Service runs the telemetry publishers + charger alert FSM. Started
// from the reactor in its own goroutine.
type Service struct {
	conn *bus.Connection

	// monotonic seq counters per topic — keeps the CM5 import side
	// able to spot stalls without reading payload contents.
	seqBattery      atomic.Uint32
	seqCharger      atomic.Uint32
	seqChargerCfg   atomic.Uint32
	seqEnvTemp      atomic.Uint32
	seqEnvHum       atomic.Uint32
	seqRuntimeMem   atomic.Uint32
	seqChargerAlert atomic.Uint32

	startedAt time.Time

	// chargerCfg carries the analog thresholds the alert FSM uses for
	// the vin_lo / vin_hi / bsr_high kinds, plus the alert mask the
	// charger config fact retains to the wire.
	chargerCfg ChargerConfig

	// alert FSM previous-bitfield state. Compared against incoming
	// values to detect bit-set transitions.
	alertFSM chargerAlertFSM

	// Retained telemetry de-chatter policy. HAL continues to sample at
	// its safety cadence; telemetry decides whether each new observation
	// is material enough to republish onto state/self/*.
	policy Policy

	batteryGate publishGate
	chargerGate publishGate
	envTempGate publishGate
	envHumGate  publishGate

	lastBattery          types.BatteryValue
	lastPublishedBattery types.BatteryValue
	lastCharger          types.ChargerValue
	lastEnvTemp          types.TemperatureValue
	lastEnvHum           types.HumidityValue

	haveBatteryObservation      bool
	havePublishedBatteryMeasure bool
	haveChargerObservation      bool
	lastBatteryPresence         batteryPresenceState

	batteryPub    retainedpub.Publisher[BatteryFact]
	chargerPub    retainedpub.Publisher[ChargerFact]
	chargerCfgPub retainedpub.Publisher[ChargerConfigFact]
	envTempPub    retainedpub.Publisher[EnvTempFact]
	envHumPub     retainedpub.Publisher[EnvHumFact]
	runtimeMemPub retainedpub.Publisher[RuntimeMemFact]
}

// New constructs the service. conn must be a fresh bus connection
// dedicated to telemetry (not shared with the updater or fabric).
func New(conn *bus.Connection) *Service {
	return NewWithPolicy(conn, DefaultPolicy)
}

// NewWithPolicy constructs the service with an explicit publication
// policy. Production normally uses New; tests and bring-up images may
// pass a shorter keepalive or different drift thresholds while the
// central config service is still absent.
func NewWithPolicy(conn *bus.Connection, policy Policy) *Service {
	s := &Service{
		conn:       conn,
		startedAt:  time.Now(),
		chargerCfg: DefaultChargerConfig(),
		policy:     policy.withDefaults(),
	}
	s.batteryPub = retainedpub.New(conn, TopicBattery, retainedpub.ComparableEqual[BatteryFact])
	s.chargerPub = retainedpub.New[ChargerFact](conn, TopicCharger, nil)
	s.chargerCfgPub = retainedpub.New(conn, TopicChargerCfg, retainedpub.ComparableEqual[ChargerConfigFact])
	s.envTempPub = retainedpub.New(conn, TopicEnvTemp, retainedpub.ComparableEqual[EnvTempFact])
	s.envHumPub = retainedpub.New(conn, TopicEnvHumidity, retainedpub.ComparableEqual[EnvHumFact])
	s.runtimeMemPub = retainedpub.New(conn, TopicRuntimeMem, retainedpub.ComparableEqual[RuntimeMemFact])
	return s
}

func (s *Service) chargerThresholds() ChargerThresholds {
	return s.chargerCfg.Thresholds
}

// Run subscribes to HAL inputs and runs the publish loop. Blocks
// until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	tempSub := s.conn.Subscribe(halEnvTemp)
	defer s.conn.Unsubscribe(tempSub)
	humSub := s.conn.Subscribe(halEnvHum)
	defer s.conn.Unsubscribe(humSub)
	pwrSub := s.conn.Subscribe(halPwrAny)
	defer s.conn.Unsubscribe(pwrSub)

	// Charger config retain at startup — the CM5-side import on
	// the CM5 keys off this for the `update.components.mcu.charger_*`
	// view, and the alert FSM analog kinds (vin_lo / vin_hi /
	// bsr_high) depend on it being present to know what to compare
	// against.
	s.publishChargerConfig()

	memTick := time.NewTicker(MemSnapshotInterval)
	defer memTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-tempSub.Channel():
			if !ok {
				continue
			}
			if v, ok := msg.Payload.(types.TemperatureValue); ok {
				s.publishEnvTemp(v)
			}
		case msg, ok := <-humSub.Channel():
			if !ok {
				continue
			}
			if v, ok := msg.Payload.(types.HumidityValue); ok {
				s.publishEnvHum(v)
			}
		case msg, ok := <-pwrSub.Channel():
			if !ok {
				continue
			}
			s.dispatchPower(&msg)
		case <-memTick.C:
			s.publishRuntimeMem()
		}
	}
}

// dispatchPower splits the power-domain wildcard into per-kind
// publish paths. Kept tiny: BatteryValue and ChargerValue are the only
// shapes we consume on this branch (TemperatureValue from
// power/temperature/internal is intentionally NOT republished — the
// canonical contract puts thermal info under environment/temperature).
func (s *Service) dispatchPower(msg *bus.Message) {
	switch v := msg.Payload.(type) {
	case types.BatteryValue:
		s.publishBattery(v)
		s.alertFSM.observeBattery(s, v)
	case types.ChargerValue:
		s.publishCharger(v)
		s.alertFSM.observe(s, v)
	case types.ChargerConfigValue:
		s.applyChargerConfig(v)
	}
}

func (s *Service) applyChargerConfig(v types.ChargerConfigValue) {
	source := v.Source
	if source == "" {
		source = "ltc4015-programmed"
	}
	s.chargerCfg = ChargerConfig{
		Source: source,
		Thresholds: ChargerThresholds{
			VinLoMV:            v.VinLo_mV,
			VinHiMV:            v.VinHi_mV,
			BSRHighUohmPerCell: v.BSRHigh_uOhmPerCell,
		},
		AlertMaskBits: v.AlertMaskBits,
	}
	s.publishChargerConfig()
}

// uptimeMs returns a service-monotonic uptime — close enough to a
// boot-uptime for the consumers' purposes (within a few HAL-init ms).
func (s *Service) uptimeMs() int64 {
	return time.Since(s.startedAt).Milliseconds()
}

// ---- retained-state publishers -------------------------------------

type batteryPresenceState struct {
	Presence          string
	MeasurementsValid bool
	Reason            string
}

func (a batteryPresenceState) equal(b batteryPresenceState) bool {
	return a.Presence == b.Presence &&
		a.MeasurementsValid == b.MeasurementsValid &&
		a.Reason == b.Reason
}

// BatteryFact is the retained payload at state/self/power/battery.
//
// The LTC4015 reports battery-missing and battery-short as charger
// state-machine faults rather than as a clean independent presence
// signal. Telemetry therefore derives presence from the latest charger
// state and omits analogue battery measurements while the pack is
// absent, faulted, or not yet known. This keeps Fabric from exporting
// meaningless floating BATSENS values as credible battery telemetry.
type BatteryFact struct {
	Presence          string `json:"presence"`
	MeasurementsValid bool   `json:"measurements_valid"`
	Reason            string `json:"reason,omitempty"`
	PackMV            int32  `json:"-"`
	PerCellMV         int32  `json:"-"`
	IBatMA            int32  `json:"-"`
	TempMC            int32  `json:"-"`
	BSRUOhmPerCell    uint32 `json:"-"`
	Seq               uint32 `json:"seq"`
	UptimeMs          int64  `json:"uptime_ms"`
}

func (f BatteryFact) MarshalJSON() ([]byte, error) {
	return f.AppendJSON(make([]byte, 0, 224)), nil
}

func (s *Service) deriveBatteryPresence() batteryPresenceState {
	if !s.haveChargerObservation {
		return batteryPresenceState{
			Presence:          "unknown",
			MeasurementsValid: false,
			Reason:            "charger_state_unknown",
		}
	}
	state := types.ChargerStateBits(s.lastCharger.State)
	if state&types.BatShortFault != 0 {
		return batteryPresenceState{
			Presence:          "fault",
			MeasurementsValid: false,
			Reason:            "bat_short_fault",
		}
	}
	if state&types.BatMissingFault != 0 {
		return batteryPresenceState{
			Presence:          "absent",
			MeasurementsValid: false,
			Reason:            "bat_missing_fault",
		}
	}
	return batteryPresenceState{
		Presence:          "present",
		MeasurementsValid: true,
	}
}

func (s *Service) publishBattery(v types.BatteryValue) {
	s.haveBatteryObservation = true
	s.lastBattery = v
	s.maybePublishBattery(time.Now())
}

func (s *Service) maybePublishBattery(now time.Time) {
	presence := s.deriveBatteryPresence()
	if presence.MeasurementsValid && !s.haveBatteryObservation {
		presence.MeasurementsValid = false
		presence.Reason = "battery_measurement_unavailable"
	}

	presenceChanged := !presence.equal(s.lastBatteryPresence)
	keepalive := s.batteryGate.shouldPublish(now, s.policy.KeepaliveInterval)
	meaningful := presence.MeasurementsValid && (!s.havePublishedBatteryMeasure || batteryMeaningful(s.lastBattery, s.lastPublishedBattery, s.policy))
	if !presenceChanged && !keepalive && !meaningful {
		return
	}

	fact := BatteryFact{
		Presence:          presence.Presence,
		MeasurementsValid: presence.MeasurementsValid,
		Reason:            presence.Reason,
		Seq:               s.seqBattery.Add(1),
		UptimeMs:          s.uptimeMs(),
	}
	if presence.MeasurementsValid {
		fact.PackMV = s.lastBattery.PackMilliV
		fact.PerCellMV = s.lastBattery.PerCellMilliV
		fact.IBatMA = s.lastBattery.IBatMilliA
		fact.TempMC = s.lastBattery.TempMilliC
		fact.BSRUOhmPerCell = s.lastBattery.BSR_uOhmPerCell
	}

	if presence.MeasurementsValid {
		s.lastPublishedBattery = s.lastBattery
		s.havePublishedBatteryMeasure = true
	}
	s.lastBatteryPresence = presence
	s.batteryGate.markPublished(now)
	_ = s.batteryPub.PublishNow(now, fact)
}

// ChargerFact is the retained payload at state/self/power/charger.
//
// The MCU publishes the compact wire shape only: analogue readings plus
// LTC4015 bitfields.  Rich decoded boolean objects are deliberately no
// longer emitted here; the CM5/Lua device layer expands state_bits,
// status_bits and system_bits after importing the raw retained fact.
type ChargerFact struct {
	VinMV      int32  `json:"vin_mV"`
	VsysMV     int32  `json:"vsys_mV"`
	IinMA      int32  `json:"iin_mA"`
	StateBits  uint16 `json:"state_bits"`
	StatusBits uint16 `json:"status_bits"`
	SystemBits uint16 `json:"system_bits"`
	Seq        uint32 `json:"seq"`
	UptimeMs   int64  `json:"uptime_ms"`
}

func (s *Service) publishCharger(v types.ChargerValue) {
	now := time.Now()
	if !s.chargerGate.shouldPublish(now, s.policy.KeepaliveInterval) && !chargerMeaningful(v, s.lastCharger, s.policy) {
		return
	}

	prevBatteryPresence := s.deriveBatteryPresence()
	fact := ChargerFact{
		VinMV:      v.VIN_mV,
		VsysMV:     v.VSYS_mV,
		IinMA:      v.IIn_mA,
		StateBits:  v.State,
		StatusBits: v.Status,
		SystemBits: v.Sys,
		Seq:        s.seqCharger.Add(1),
		UptimeMs:   s.uptimeMs(),
	}
	s.lastCharger = v
	s.haveChargerObservation = true
	s.chargerGate.markPublished(now)
	_ = s.chargerPub.PublishNow(now, fact)

	// Charger-state faults are the best battery-presence signal the
	// LTC4015 gives us. When that derived presence changes, refresh the
	// battery retained fact. For an observed-present charger with no
	// battery telemetry yet, wait for the next BatteryValue so the first
	// present fact can carry measurements. For absent/fault/unknown, publish
	// immediately even without a BatteryValue because the useful fact is the
	// absence itself.
	nextBatteryPresence := s.deriveBatteryPresence()
	if !nextBatteryPresence.equal(prevBatteryPresence) {
		s.maybePublishBattery(now)
	}
}

// ChargerConfigFact — state/self/power/charger/config. Effective
// LTC4015 configuration. Strictly forbidden from carrying
// operating-state booleans (charger_enabled, ok_to_charge, etc.) —
// those live on state/self/power/charger.
type ChargerConfigFact struct {
	Schema        string            `json:"schema"`
	Source        string            `json:"source"`
	Thresholds    ChargerThresholds `json:"thresholds"`
	AlertMaskBits uint16            `json:"alert_mask_bits"`
	AlertMask     ChargerAlertMask  `json:"alert_mask"`
	Seq           uint32            `json:"seq"`
	UptimeMs      int64             `json:"uptime_ms"`
}

func (s *Service) publishChargerConfig() {
	cfg := s.chargerCfg
	source := cfg.Source
	if source == "" {
		// Defensive: a caller that constructed ChargerConfig without
		// going through DefaultChargerConfig may have left this empty.
		// Make the gap visible on the wire rather than misreporting.
		source = "ltc4015-default"
	}
	fact := ChargerConfigFact{
		Schema:        "charger-config/1",
		Source:        source,
		Thresholds:    cfg.Thresholds,
		AlertMaskBits: cfg.AlertMaskBits,
		AlertMask:     cfg.AlertMask,
		Seq:           s.seqChargerCfg.Add(1),
		UptimeMs:      s.uptimeMs(),
	}
	_ = s.chargerCfgPub.PublishNow(time.Now(), fact)
}

// EnvTempFact — state/self/environment/temperature.
type EnvTempFact struct {
	DeciC    int32  `json:"deci_c"`
	Seq      uint32 `json:"seq"`
	UptimeMs int64  `json:"uptime_ms"`
}

func (s *Service) publishEnvTemp(v types.TemperatureValue) {
	now := time.Now()
	deciC := int32(v.DeciC)
	lastDeciC := int32(s.lastEnvTemp.DeciC)
	if !s.envTempGate.shouldPublish(now, s.policy.KeepaliveInterval) && !meaningfulDeltaI32(deciC, lastDeciC, s.policy.EnvTempMinDeltaDeciC, s.policy.EnvTempMinDeltaPct) {
		return
	}
	fact := EnvTempFact{
		DeciC:    deciC,
		Seq:      s.seqEnvTemp.Add(1),
		UptimeMs: s.uptimeMs(),
	}
	s.lastEnvTemp = v
	s.envTempGate.markPublished(now)
	_ = s.envTempPub.PublishNow(now, fact)
}

// EnvHumFact — state/self/environment/humidity.
type EnvHumFact struct {
	RHx100   int32  `json:"rh_x100"`
	Seq      uint32 `json:"seq"`
	UptimeMs int64  `json:"uptime_ms"`
}

func (s *Service) publishEnvHum(v types.HumidityValue) {
	now := time.Now()
	rh := int32(v.RHx100)
	lastRH := int32(s.lastEnvHum.RHx100)
	if !s.envHumGate.shouldPublish(now, s.policy.KeepaliveInterval) && !meaningfulDeltaI32(rh, lastRH, s.policy.EnvHumMinDeltaRHx100, s.policy.EnvHumMinDeltaPct) {
		return
	}
	fact := EnvHumFact{
		RHx100:   rh,
		Seq:      s.seqEnvHum.Add(1),
		UptimeMs: s.uptimeMs(),
	}
	s.lastEnvHum = v
	s.envHumGate.markPublished(now)
	_ = s.envHumPub.PublishNow(now, fact)
}

// RuntimeMemFact — state/self/runtime/memory. Sourced from
// runtime.MemStats.Alloc; sufficient for the retained-fact
// "memory pressure" signal Lua consumers expect.
type RuntimeMemFact struct {
	AllocBytes uint64 `json:"alloc_bytes"`
	Seq        uint32 `json:"seq"`
	UptimeMs   int64  `json:"uptime_ms"`
}

func (s *Service) publishRuntimeMem() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fact := RuntimeMemFact{
		AllocBytes: ms.Alloc,
		Seq:        s.seqRuntimeMem.Add(1),
		UptimeMs:   s.uptimeMs(),
	}
	_ = s.runtimeMemPub.PublishNow(time.Now(), fact)
}

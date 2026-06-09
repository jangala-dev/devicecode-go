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
	"encoding/json"
	"runtime"
	"sync/atomic"
	"time"

	"devicecode-go/bus"
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

	// TopicFabricLink mirrors the updater's link-ready watcher: telemetry
	// republishes the charger config retain on every link-ready edge
	// so the CM5 sees a fresh config fact on every newly established
	// session, warm or cold. (Per-value retains like
	// state/self/power/battery refresh naturally on the next HAL
	// publish; the static-ish config fact needs an explicit re-emit.)
	topicFabricLink = bus.T("state", "fabric", "link", "+")
)

// HAL source topics — single point of truth for what we subscribe to.
var (
	halEnvTemp = bus.T("hal", "cap", "env", string(types.KindTemperature), "core", "value")
	halEnvHum  = bus.T("hal", "cap", "env", string(types.KindHumidity), "core", "value")
	halPwrAny  = bus.T("hal", "cap", "power", "+", "internal", "value")
)

// MemSnapshotInterval is how often the runtime/memory fact republishes.
// Keep it on the order of the existing reactor mem-stat cadence to
// avoid burning UART bandwidth on changes that don't affect anything.
const MemSnapshotInterval = 3 * time.Second

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
}

// New constructs the service. conn must be a fresh bus connection
// dedicated to telemetry (not shared with the updater or fabric).
func New(conn *bus.Connection) *Service {
	return &Service{
		conn:       conn,
		startedAt:  time.Now(),
		chargerCfg: DefaultChargerConfig(),
	}
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

	linkSub := s.conn.Subscribe(topicFabricLink)
	defer s.conn.Unsubscribe(linkSub)

	memTick := time.NewTicker(MemSnapshotInterval)
	defer memTick.Stop()

	linkState := map[string]linkObservation{}

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-tempSub.Channel():
			if !ok || msg == nil {
				continue
			}
			if v, ok := msg.Payload.(types.TemperatureValue); ok {
				s.publishEnvTemp(v)
			}
		case msg, ok := <-humSub.Channel():
			if !ok || msg == nil {
				continue
			}
			if v, ok := msg.Payload.(types.HumidityValue); ok {
				s.publishEnvHum(v)
			}
		case msg, ok := <-pwrSub.Channel():
			if !ok || msg == nil {
				continue
			}
			s.dispatchPower(msg)
		case msg, ok := <-linkSub.Channel():
			if !ok || msg == nil {
				continue
			}
			linkID, obs := decodeLinkReady(msg)
			if linkID == "" {
				continue
			}
			prev, hadPrev := linkState[linkID]
			if linkReadyEdgeReason(prev, obs, hadPrev) != "" {
				s.publishChargerConfig()
			}
			linkState[linkID] = obs
		case <-memTick.C:
			s.publishRuntimeMem()
		}
	}
}

// decodeLinkReady mirrors services/updater's helper but local to the
// telemetry package — kept duplicated rather than reaching into
// updater (cleaner package boundary).
type linkObservation struct {
	Ready    bool
	PeerSID  string
	LocalSID string
}

// fabricLinkObserver is implemented by services/fabric's retained link-state
// payload. Keeping this as a tiny structural interface avoids JSON reflection
// in the common in-process TinyGo path while still tolerating map/JSON payloads
// in host-side tests.
type fabricLinkObserver interface {
	FabricLinkObservation() (ready bool, peerSID string, localSID string)
}

func linkReadyEdgeReason(prev, cur linkObservation, hadPrev bool) string {
	if !cur.Ready {
		return ""
	}
	if !hadPrev || !prev.Ready {
		return "ready_edge"
	}
	if prev.PeerSID != cur.PeerSID {
		return "peer_sid_changed"
	}
	if prev.LocalSID != cur.LocalSID {
		return "local_sid_changed"
	}
	return ""
}

func decodeLinkReady(msg *bus.Message) (string, linkObservation) {
	var obs linkObservation
	if msg == nil {
		return "", obs
	}
	t := msg.Topic
	if t == nil || t.Len() < 4 {
		return "", obs
	}
	id, _ := t.At(t.Len() - 1).(string)
	if id == "" {
		return "", obs
	}
	switch p := msg.Payload.(type) {
	case nil:
		return id, obs
	case map[string]any:
		obs.Ready, _ = p["ready"].(bool)
		obs.PeerSID, _ = p["peer_sid"].(string)
		obs.LocalSID, _ = p["local_sid"].(string)
		return id, obs
	case fabricLinkObserver:
		obs.Ready, obs.PeerSID, obs.LocalSID = p.FabricLinkObservation()
		return id, obs
	}
	// Probe via JSON for the typed-struct payload fabric publishes.
	b, err := json.Marshal(msg.Payload)
	if err != nil {
		return id, obs
	}
	var probe struct {
		Ready    bool   `json:"ready"`
		PeerSID  string `json:"peer_sid"`
		LocalSID string `json:"local_sid"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return id, obs
	}
	obs.Ready = probe.Ready
	obs.PeerSID = probe.PeerSID
	obs.LocalSID = probe.LocalSID
	return id, obs
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

// BatteryFact is the retained payload at state/self/power/battery.
// All units are integer engineering units per the spec.
type BatteryFact struct {
	PackMV         int32  `json:"pack_mV"`
	PerCellMV      int32  `json:"per_cell_mV"`
	IBatMA         int32  `json:"ibat_mA"`
	TempMC         int32  `json:"temp_mC"`
	BSRUOhmPerCell uint32 `json:"bsr_uohm_per_cell"`
	Seq            uint32 `json:"seq"`
	UptimeMs       int64  `json:"uptime_ms"`
}

func (s *Service) publishBattery(v types.BatteryValue) {
	fact := BatteryFact{
		PackMV:         v.PackMilliV,
		PerCellMV:      v.PerCellMilliV,
		IBatMA:         v.IBatMilliA,
		TempMC:         v.TempMilliC,
		BSRUOhmPerCell: v.BSR_uOhmPerCell,
		Seq:            s.seqBattery.Add(1),
		UptimeMs:       s.uptimeMs(),
	}
	s.conn.Publish(s.conn.NewMessage(TopicBattery, fact, true))
}

// ChargerFact is the retained payload at state/self/power/charger.
// Carries raw bitfields AND 3 decoded boolean objects.
//
// The canonical key names below come from
// ../docs/updating.md. They are NOT the existing display names in
// types.ChargerStateTable etc.
// (those drop the `_charge` / `_active` / `_fault` suffixes for
// log-line brevity). The wire-canonical names are spec-frozen because
// the Lua import side keys off them; renaming any of these is a
// wire-break.
type ChargerFact struct {
	VinMV      int32           `json:"vin_mV"`
	VsysMV     int32           `json:"vsys_mV"`
	IinMA      int32           `json:"iin_mA"`
	StateBits  uint16          `json:"state_bits"`
	StatusBits uint16          `json:"status_bits"`
	SystemBits uint16          `json:"system_bits"`
	State      map[string]bool `json:"state"`
	Status     map[string]bool `json:"status"`
	System     map[string]bool `json:"system"`
	Seq        uint32          `json:"seq"`
	UptimeMs   int64           `json:"uptime_ms"`
}

// Canonical name tables. Each entry is a (bit, canonical-name) pair.
// Counts match the spec's "27 booleans total: 11 + 4 + 12".
var chargerStateNames = []struct {
	bit  types.ChargerStateBits
	name string
}{
	{types.EqualizeCharge, "equalize_charge"},
	{types.AbsorbCharge, "absorb_charge"},
	{types.ChargerSuspended, "charger_suspended"},
	{types.Precharge, "precharge"},
	{types.CCCVCharge, "cccv_charge"},
	{types.NTCPause, "ntc_pause"},
	{types.TimerTerm, "timer_term"},
	{types.COverXTerm, "c_over_x_term"},
	{types.MaxChargeTimeFault, "max_charge_time_fault"},
	{types.BatMissingFault, "bat_missing_fault"},
	{types.BatShortFault, "bat_short_fault"},
}

var chargerStatusNames = []struct {
	bit  types.ChargeStatusBits
	name string
}{
	{types.VinUvclActive, "vin_uvcl_active"},
	{types.IinLimitActive, "iin_limit_active"},
	{types.ConstCurrent, "const_current"},
	{types.ConstVoltage, "const_voltage"},
}

var chargerSystemNames = []struct {
	bit  types.SystemStatus
	name string
}{
	{types.ChargerEnabled, "charger_enabled"},
	{types.MpptEnPin, "mppt_en_pin"},
	{types.EqualizeReq, "equalize_req"},
	{types.DrvccGood, "drvcc_good"},
	{types.CellCountError, "cell_count_error"},
	{types.OkToCharge, "ok_to_charge"},
	{types.NoRt, "no_rt"},
	{types.ThermalShutdown, "thermal_shutdown"},
	{types.VinOvlo, "vin_ovlo"},
	{types.VinGtVbat, "vin_gt_vbat"},
	{types.IntvccGt4p3V, "intvcc_gt_4p3v"},
	{types.IntvccGt2p8V, "intvcc_gt_2p8v"},
}

func decodeChargerState(v uint16) map[string]bool {
	out := make(map[string]bool, len(chargerStateNames))
	for _, e := range chargerStateNames {
		out[e.name] = (v & uint16(e.bit)) != 0
	}
	return out
}

func decodeChargerStatus(v uint16) map[string]bool {
	out := make(map[string]bool, len(chargerStatusNames))
	for _, e := range chargerStatusNames {
		out[e.name] = (v & uint16(e.bit)) != 0
	}
	return out
}

func decodeChargerSystem(v uint16) map[string]bool {
	out := make(map[string]bool, len(chargerSystemNames))
	for _, e := range chargerSystemNames {
		out[e.name] = (v & uint16(e.bit)) != 0
	}
	return out
}

func (s *Service) publishCharger(v types.ChargerValue) {
	fact := ChargerFact{
		VinMV:      v.VIN_mV,
		VsysMV:     v.VSYS_mV,
		IinMA:      v.IIn_mA,
		StateBits:  v.State,
		StatusBits: v.Status,
		SystemBits: v.Sys,
		State:      decodeChargerState(v.State),
		Status:     decodeChargerStatus(v.Status),
		System:     decodeChargerSystem(v.Sys),
		Seq:        s.seqCharger.Add(1),
		UptimeMs:   s.uptimeMs(),
	}
	s.conn.Publish(s.conn.NewMessage(TopicCharger, fact, true))
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
	s.conn.Publish(s.conn.NewMessage(TopicChargerCfg, fact, true))
}

// EnvTempFact — state/self/environment/temperature.
type EnvTempFact struct {
	DeciC    int32  `json:"deci_c"`
	Seq      uint32 `json:"seq"`
	UptimeMs int64  `json:"uptime_ms"`
}

func (s *Service) publishEnvTemp(v types.TemperatureValue) {
	fact := EnvTempFact{
		DeciC:    int32(v.DeciC),
		Seq:      s.seqEnvTemp.Add(1),
		UptimeMs: s.uptimeMs(),
	}
	s.conn.Publish(s.conn.NewMessage(TopicEnvTemp, fact, true))
}

// EnvHumFact — state/self/environment/humidity.
type EnvHumFact struct {
	RHx100   int32  `json:"rh_x100"`
	Seq      uint32 `json:"seq"`
	UptimeMs int64  `json:"uptime_ms"`
}

func (s *Service) publishEnvHum(v types.HumidityValue) {
	fact := EnvHumFact{
		RHx100:   int32(v.RHx100),
		Seq:      s.seqEnvHum.Add(1),
		UptimeMs: s.uptimeMs(),
	}
	s.conn.Publish(s.conn.NewMessage(TopicEnvHumidity, fact, true))
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
	s.conn.Publish(s.conn.NewMessage(TopicRuntimeMem, fact, true))
}

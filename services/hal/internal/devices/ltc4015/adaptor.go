// services/hal/internal/devices/ltc4015/adaptor.go
package ltc4015

import (
	"context"
	"errors"
	"time"

	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/halerr"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/services/hal/internal/util"
)

// ---------------- Params supplied via config ----------------

type Params struct {
	Addr            int    `json:"addr,omitempty"`
	Cells           uint8  `json:"cells,omitempty"`
	Chem            string `json:"chem,omitempty"` // "auto" (default), "lithium", "lead_acid"
	RSNSB_uOhm      uint32 `json:"rsnsb_uohm,omitempty"`
	RSNSI_uOhm      uint32 `json:"rsnsi_uohm,omitempty"`
	TargetsWritable *bool  `json:"targets_writable,omitempty"`
	QCountPrescale  uint16 `json:"qcount_prescale,omitempty"`
	SampleEveryMS   int    `json:"sample_every_ms,omitempty"`
	SMBAlertPin     *int   `json:"smbalert_pin,omitempty"`
	IRQDebounceMS   int    `json:"irq_debounce_ms,omitempty"`
	ForceMeasSysOn  *bool  `json:"force_meas_sys_on,omitempty"`
	EnableQCount    *bool  `json:"enable_qcount,omitempty"`
}

// Internal chemistry enum (platform wrappers translate to chip-specific values).
type chemistry uint8

const (
	chemUnknown chemistry = iota
	chemLithium
	chemLeadAcid
)

func parseChem(s string) chemistry {
	switch s {
	case "lithium":
		return chemLithium
	case "lead_acid":
		return chemLeadAcid
	default:
		return chemUnknown // includes "", "auto"
	}
}

// Config passed to platform factory; wrappers convert as needed.
type configLite struct {
	Address         uint16
	RSNSB_uOhm      uint32
	RSNSI_uOhm      uint32
	Cells           uint8
	Chem            chemistry
	QCountPrescale  uint16
	TargetsWritable bool
	ForceMeasSysOn  bool
	EnableQCount    bool
}

// Summary used by adaptor (device-agnostic).
type StatusSummary struct {
	Equalize, Absorb, Precharge, Suspended bool
	InCCCV, CC, CV                         bool
	OkToCharge                             bool
	BatMissing, BatShort, ThermalShutdown  bool
	VinUvcl, IinLimit                      bool
}

// Raw alert/status payloads (uint16 bitfields, chip-agnostic at adaptor level).
type AlertEventRaw struct{ Limit, ChgState, ChgStatus uint16 }

// Chemistry-specific helper surface used by applyProfile.
type laView interface {
	SetVChargeSetting_mVPerCell(int32, bool) error
	SetVAbsorbDelta_mVPerCell(int32) error
	SetVEqualizeDelta_mVPerCell(int32) error
	SetMaxAbsorbTime_s(uint16) error
	SetEqualizeTime_s(uint16) error
	EnableLeadAcidTempComp(bool) error
}

// ltcDev is the platform-neutral interface the adaptor depends upon.
type ltcDev interface {
	// lifecycle/config
	Configure(configLite) error
	Chemistry() string // "lithium" | "lead_acid" | "unknown"
	Cells() uint8

	// telemetry readiness
	MeasSystemValid() (bool, error)

	// telemetry
	BatteryMilliVPerCell() (int32, error)
	BatteryMilliVPack() (int32, error)
	VinMilliV() (int32, error)
	VsysMilliV() (int32, error)
	IbatMilliA() (int32, error)
	IinMilliA() (int32, error)
	DieMilliC() (int32, error)
	BSRMicroOhmPerCell() (uint32, error)
	IChargeDAC_mA() (int32, error)
	IinLimitDAC_mA() (int32, error)
	IChargeBSR_mA() (int32, error)

	// status
	Summary() (StatusSummary, error)
	RawStatus() (systemStatus uint16, chargerState uint16, chargeStatus uint16, err error)

	// controls
	SetIinLimit_mA(int32) error
	SetIChargeTarget_mA(int32) error
	SetVinUvcl_mV(int32) error

	// sequencing
	SetSuspend(on bool) error

	// alerts
	DrainAlerts() (AlertEventRaw, error)
	ServiceSMBAlert() (AlertEventRaw, bool, error)
	AlertActive(get func() bool) bool

	// chemistry views
	LeadAcid() (laView, bool)
}

// Register builder.
func init() { registry.RegisterBuilder("ltc4015", builder{}) }

type builder struct{}

func (builder) Build(in registry.BuildInput) (registry.BuildOutput, error) {
	if in.BusRefType != "i2c" || in.BusRefID == "" {
		return registry.BuildOutput{}, halerr.ErrMissingBusRef
	}
	i2c, ok := in.Buses.ByID(in.BusRefID)
	if !ok {
		return registry.BuildOutput{}, halerr.ErrUnknownBus
	}

	var p Params
	if err := util.DecodeJSON(in.ParamsJSON, &p); err != nil {
		return registry.BuildOutput{}, err
	}

	cfg := configLite{
		Address:         uint16(p.Addr),
		RSNSB_uOhm:      p.RSNSB_uOhm,
		RSNSI_uOhm:      p.RSNSI_uOhm,
		Cells:           p.Cells,
		Chem:            parseChem(p.Chem),
		QCountPrescale:  p.QCountPrescale,
		TargetsWritable: p.TargetsWritable == nil || *p.TargetsWritable,
		ForceMeasSysOn:  p.ForceMeasSysOn != nil && *p.ForceMeasSysOn,
		EnableQCount:    p.EnableQCount != nil && *p.EnableQCount,
	}

	// Platform factory constructs real or simulated device.
	dev, err := newLTC4015(i2c, cfg)
	if err != nil {
		return registry.BuildOutput{}, err
	}
	if err := dev.Configure(cfg); err != nil {
		return registry.BuildOutput{}, err
	}

	ad := &adaptor{
		id:               in.DeviceID,
		dev:              dev,
		haveB:            p.RSNSB_uOhm != 0,
		haveI:            p.RSNSI_uOhm != 0,
		targetsWritable:  cfg.TargetsWritable,
		extensionsBitMap: ltc4015BitfieldsMap(), // retained once via Capabilities()
	}

	out := registry.BuildOutput{
		Adaptor:     ad,
		BusID:       in.BusRefID,
		SampleEvery: time.Duration(util.ClampInt(p.SampleEveryMS, 0, 3_600_000)) * time.Millisecond,
	}
	if out.SampleEvery <= 0 {
		out.SampleEvery = 2 * time.Second
	}

	// Optional SMBALERT# IRQ (falling edge, active-low line).
	if p.SMBAlertPin != nil {
		if pin, ok := in.Pins.ByNumber(*p.SMBAlertPin); ok {
			ad.getAlert = pin.Get
			if irqPin, ok := pin.(halcore.IRQPin); ok {
				db := util.ClampInt(p.IRQDebounceMS, 0, 50)
				out.IRQ = &registry.IRQRequest{
					DevID:      in.DeviceID,
					Pin:        irqPin,
					Edge:       halcore.EdgeFalling,
					DebounceMS: db,
					Invert:     false,
				}
			}
		}
	}

	return out, nil
}

type adaptor struct {
	id               string
	dev              ltcDev
	haveB            bool
	haveI            bool
	getAlert         func() bool // optional
	targetsWritable  bool
	extensionsBitMap map[string]any
}

func (a *adaptor) ID() string { return a.id }

func (a *adaptor) Capabilities() []halcore.CapInfo {
	// Power info with units (vendor-neutral).
	powerInfo := map[string]any{
		"schema_version": 2,
		"driver":         "ltc4015",
		"cells":          a.dev.Cells(),
		"chemistry":      a.dev.Chemistry(),
		"units": map[string]any{
			"vbat_per_cell_mV":  "mV",
			"vbat_pack_mV":      "mV",
			"vin_mV":            "mV",
			"vsys_mV":           "mV",
			"ibat_mA":           "mA",
			"iin_mA":            "mA",
			"die_mC":            "m°C",
			"bsr_uohm_per_cell": "µΩ",
			"icharge_dac_mA":    "mA",
			"iin_limit_dac_mA":  "mA",
			"icharge_bsr_mA":    "mA",
		},
	}

	// Charger info with optional vendor extension documenting bitfields.
	chargerInfo := map[string]any{
		"schema_version":   2,
		"model":            "ltc4015",
		"chemistry":        a.dev.Chemistry(),
		"cells":            a.dev.Cells(),
		"targets_writable": a.targetsWritable,
		"extensions": map[string]any{
			"ltc4015": map[string]any{
				"bitfields": a.extensionsBitMap, // retained map: decode "raw" masks
			},
		},
	}

	alertsInfo := map[string]any{
		"schema_version": 1,
		"groups":         []string{"limit", "chg_state", "chg_status"},
	}

	return []halcore.CapInfo{
		{Kind: "power", Info: powerInfo},
		{Kind: "charger", Info: chargerInfo},
		{Kind: "alerts", Info: alertsInfo},
	}
}

// Trigger/Collect: LTC4015 telemetry is ready continuously; no start/convert wait.
func (a *adaptor) Trigger(ctx context.Context) (time.Duration, error) { return 0, nil }

func (a *adaptor) Collect(ctx context.Context) (halcore.Sample, error) {
	now := time.Now().UnixMilli()
	var out halcore.Sample

	// Drain SMBALERT# while asserted (bounded). Respect context cancellation.
	if a.getAlert != nil && a.dev.AlertActive(a.getAlert) {
		for i := 0; i < 4 && a.dev.AlertActive(a.getAlert); i++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ev, ok, err := a.dev.ServiceSMBAlert()
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			} // different device responded on ARA
			if ev.Limit != 0 || ev.ChgState != 0 || ev.ChgStatus != 0 {
				out = append(out, halcore.Reading{
					Kind: "alerts",
					Payload: map[string]any{
						"limit": ev.Limit, "chg_state": ev.ChgState, "chg_status": ev.ChgStatus, "ts_ms": now,
					},
					TsMs: now,
				})
			}
			time.Sleep(200 * time.Microsecond)
		}
	}

	// If measurement system is not ready, request a retry via worker back-off.
	if ok, err := a.dev.MeasSystemValid(); err == nil && !ok {
		return nil, halcore.ErrNotReady
	}

	// Power (device-agnostic keys).
	p := map[string]any{"ts_ms": now}
	if v, err := a.dev.BatteryMilliVPerCell(); err == nil {
		p["vbat_per_cell_mV"] = v
	}
	if v, err := a.dev.BatteryMilliVPack(); err == nil {
		p["vbat_pack_mV"] = v
	}
	if v, err := a.dev.VinMilliV(); err == nil {
		p["vin_mV"] = v
	}
	if v, err := a.dev.VsysMilliV(); err == nil {
		p["vsys_mV"] = v
	}
	if a.haveB {
		if v, err := a.dev.IbatMilliA(); err == nil {
			p["ibat_mA"] = v
		}
		if v, err := a.dev.IChargeDAC_mA(); err == nil {
			p["icharge_dac_mA"] = v
		}
		if v, err := a.dev.BSRMicroOhmPerCell(); err == nil {
			p["bsr_uohm_per_cell"] = v
		}
		if v, err := a.dev.IChargeBSR_mA(); err == nil {
			p["icharge_bsr_mA"] = v
		}
	}
	if a.haveI {
		if v, err := a.dev.IinMilliA(); err == nil {
			p["iin_mA"] = v
		}
		if v, err := a.dev.IinLimitDAC_mA(); err == nil {
			p["iin_limit_dac_mA"] = v
		}
	}
	if v, err := a.dev.DieMilliC(); err == nil {
		p["die_mC"] = v
	}
	out = append(out, halcore.Reading{Kind: "power", Payload: p, TsMs: now})

	// Charger summary + optional raw bitfields.
	sum, _ := a.dev.Summary()
	ss, cs, st, _ := a.dev.RawStatus()
	out = append(out, halcore.Reading{
		Kind: "charger",
		Payload: map[string]any{
			"phase": phaseFrom(sum),
			"input_limited": map[string]any{
				"vin_uvcl":  sum.VinUvcl,
				"iin_limit": sum.IinLimit,
			},
			"ok_to_charge": sum.OkToCharge,
			"faults": map[string]any{
				"bat_missing":      sum.BatMissing,
				"bat_short":        sum.BatShort,
				"thermal_shutdown": sum.ThermalShutdown,
			},
			// Vendor extension: compact raw bitfields (decode using retained map).
			"raw":   map[string]any{"system_status": ss, "charger_state": cs, "charge_status": st},
			"ts_ms": now,
		},
		TsMs: now,
	})

	return out, nil
}

func phaseFrom(s StatusSummary) string {
	switch {
	case s.BatMissing || s.BatShort || s.ThermalShutdown:
		return "fault"
	case s.Equalize:
		return "equalize"
	case s.Absorb:
		return "absorb"
	case s.Precharge:
		return "precharge"
	case s.InCCCV:
		if s.CC {
			return "cc"
		}
		if s.CV {
			return "cv"
		}
		return "cc"
	case s.Suspended:
		return "suspended"
	default:
		return "idle"
	}
}

// Controls (device-agnostic).
func (a *adaptor) Control(kind, method string, payload any) (any, error) {
	if kind != "charger" {
		return nil, halcore.ErrUnsupported
	}
	switch method {
	case "set_input_current_limit":
		if mA, ok := getInt(payload, "mA"); ok {
			return okReply(a.dev.SetIinLimit_mA(int32(mA)))
		}
		return nil, badPayload("mA")
	case "set_charge_current":
		if mA, ok := getInt(payload, "mA"); ok {
			return okReply(a.dev.SetIChargeTarget_mA(int32(mA)))
		}
		return nil, badPayload("mA")
	case "set_vin_uvcl":
		if mV, ok := getInt(payload, "mV"); ok {
			return okReply(a.dev.SetVinUvcl_mV(int32(mV)))
		}
		return nil, badPayload("mV")
	case "apply_profile":
		return a.applyProfile(payload)
	case "read_alerts":
		ev, err := a.dev.DrainAlerts()
		if err != nil {
			return nil, err
		}
		return map[string]any{"limit": ev.Limit, "chg_state": ev.ChgState, "chg_status": ev.ChgStatus}, nil
	default:
		return nil, halcore.ErrUnsupported
	}
}

func badPayload(_ string) error { // keep message small like other HAL errors
	return errors.New("invalid_payload")
}

func (a *adaptor) applyProfile(p any) (any, error) {
	m, _ := p.(map[string]any)
	if cells, ok := getInt(m, "cells"); ok && cells > 0 {
		_ = a.dev.Configure(configLite{Cells: uint8(cells)}) // best-effort
	}

	chem, _ := getString(m, "chemistry")
	_ = a.dev.SetSuspend(true)
	defer a.dev.SetSuspend(false)

	switch chem {
	case "lead_acid":
		if la, ok := a.dev.LeadAcid(); ok {
			if vpc, ok := getInt(m, "vcharge_mV_per_cell"); ok {
				if err := la.SetVChargeSetting_mVPerCell(int32(vpc), getBool(m, "temp_comp")); err != nil {
					return nil, err
				}
			}
			if d, ok := getInt(m, "vabsorb_delta_mV"); ok {
				if err := la.SetVAbsorbDelta_mVPerCell(int32(d)); err != nil {
					return nil, err
				}
			}
			if d, ok := getInt(m, "vequalize_delta_mV"); ok {
				if err := la.SetVEqualizeDelta_mVPerCell(int32(d)); err != nil {
					return nil, err
				}
			}
			if s, ok := getInt(m, "max_absorb_time_s"); ok {
				if err := la.SetMaxAbsorbTime_s(uint16(s)); err != nil {
					return nil, err
				}
			}
			if s, ok := getInt(m, "equalize_time_s"); ok {
				if err := la.SetEqualizeTime_s(uint16(s)); err != nil {
					return nil, err
				}
			}
			if _, present := m["temp_comp"]; present {
				if err := la.EnableLeadAcidTempComp(getBool(m, "temp_comp")); err != nil {
					return nil, err
				}
			}
		}
	case "lithium":
		if mA, ok := getInt(m, "icharge_mA"); ok {
			if err := a.dev.SetIChargeTarget_mA(int32(mA)); err != nil {
				return nil, err
			}
		}
	}

	ss, cs, st, _ := a.dev.RawStatus()
	return map[string]any{"ok": true, "raw": map[string]any{"system_status": ss, "charger_state": cs, "charge_status": st}}, nil
}

// Small helpers.
func okReply(err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}
func getInt(p any, key string) (int, bool) {
	if m, ok := p.(map[string]any); ok {
		switch v := m[key].(type) {
		case int:
			return v, true
		case int64:
			return int(v), true
		case float64:
			return int(v), true
		}
	}
	return 0, false
}
func getString(m map[string]any, key string) (string, bool) {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}
func getBool(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// Retained vendor bitfield dictionary for decoding "raw" masks.
func ltc4015BitfieldsMap() map[string]any {
	return map[string]any{
		"system_status": map[int]string{
			0:  "intvcc_gt_2p8v",
			1:  "intvcc_gt_4p3v",
			2:  "vin_gt_vbat",
			3:  "vin_ovlo",
			4:  "thermal_shutdown",
			5:  "no_rt",
			6:  "ok_to_charge",
			8:  "cell_count_error",
			9:  "drvcc_good",
			10: "equalize_req",
			11: "mppt_en_pin",
			13: "charger_enabled",
		},
		"charger_state": map[int]string{
			0:  "bat_short_fault",
			1:  "bat_missing_fault",
			2:  "max_charge_time_fault",
			3:  "c_over_x_term",
			4:  "timer_term",
			5:  "ntc_pause",
			6:  "cccv_charge",
			7:  "precharge",
			8:  "charger_suspended",
			9:  "absorb_charge",
			10: "equalize_charge",
		},
		"charge_status": map[int]string{
			0: "const_voltage",
			1: "const_current",
			2: "iin_limit_active",
			3: "vin_uvcl_active",
		},
	}
}

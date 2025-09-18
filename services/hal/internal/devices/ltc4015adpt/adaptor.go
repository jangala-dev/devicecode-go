// services/hal/internal/devices/ltc4015/adaptor.go
package ltc4015adpt

import (
	"context"
	"time"

	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/halerr"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/services/hal/internal/util"
	"devicecode-go/types"
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
	// Power info
	powerInfo := types.PowerInfo{
		SchemaVersion: 1,
		Driver:        "ltc4015",
		Cells:         a.dev.Cells(),
		Chemistry:     parseChemistry(a.dev.Chemistry()),
		Units: types.PowerUnits{
			VBatPerCell_mV:  "mV",
			VBatPack_mV:     "mV",
			Vin_mV:          "mV",
			Vsys_mV:         "mV",
			IBat_mA:         "mA",
			IIn_mA:          "mA",
			Die_mC:          "m°C",
			BSR_uohmPerCell: "µΩ",
			IChargeDAC_mA:   "mA",
			IInLimitDAC_mA:  "mA",
			IChargeBSR_mA:   "mA",
		},
	}
	// Charger info inc. vendor bitfield dictionary
	chargerInfo := types.ChargerInfo{
		SchemaVersion:   2,
		Model:           "ltc4015",
		Chemistry:       parseChemistry(a.dev.Chemistry()),
		Cells:           a.dev.Cells(),
		TargetsWritable: a.targetsWritable,
		VendorBitfields: types.ChargerBitfields{
			SystemStatus: ltc4015BitfieldsMap()["system_status"].(map[int]string),
			ChargerState: ltc4015BitfieldsMap()["charger_state"].(map[int]string),
			ChargeStatus: ltc4015BitfieldsMap()["charge_status"].(map[int]string),
		},
	}
	alertsInfo := types.AlertsInfo{SchemaVersion: 1, Groups: []string{"limit", "chg_state", "chg_status"}}

	return []halcore.CapInfo{
		{Kind: "power", Info: powerInfo},
		{Kind: "charger", Info: chargerInfo},
		{Kind: "alerts", Info: alertsInfo},
	}
}

// Trigger/Collect: LTC4015 telemetry is ready continuously; no start/convert wait.
func (a *adaptor) Trigger(ctx context.Context) (time.Duration, error) { return 0, nil }

func (a *adaptor) Collect(ctx context.Context) (halcore.Sample, error) {
	nowt := time.Now()
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
					Kind:    "alerts",
					Payload: types.AlertsEvent{Limit: ev.Limit, ChgState: ev.ChgState, ChgStatus: ev.ChgStatus, TS: nowt},
				})
			}
			time.Sleep(200 * time.Microsecond)
		}
	}

	// If measurement system is not ready, request a retry via worker back-off.
	if ok, err := a.dev.MeasSystemValid(); err == nil && !ok {
		return nil, halcore.ErrNotReady
	}

	// Power
	pv := types.PowerValue{TS: nowt}
	if v, err := a.dev.BatteryMilliVPerCell(); err == nil {
		pv.VBatPerCell_mV = &v
	}
	if v, err := a.dev.BatteryMilliVPack(); err == nil {
		pv.VBatPack_mV = &v
	}
	if v, err := a.dev.VinMilliV(); err == nil {
		pv.Vin_mV = &v
	}
	if v, err := a.dev.VsysMilliV(); err == nil {
		pv.Vsys_mV = &v
	}
	if a.haveB {
		if v, err := a.dev.IbatMilliA(); err == nil {
			pv.IBat_mA = &v
		}
		if v, err := a.dev.IChargeDAC_mA(); err == nil {
			pv.IChargeDAC_mA = &v
		}
		if v, err := a.dev.BSRMicroOhmPerCell(); err == nil {
			pv.BSR_uohmPerCell = &v
		}
		if v, err := a.dev.IChargeBSR_mA(); err == nil {
			pv.IChargeBSR_mA = &v
		}
	}
	if a.haveI {
		if v, err := a.dev.IinMilliA(); err == nil {
			pv.IIn_mA = &v
		}
		if v, err := a.dev.IinLimitDAC_mA(); err == nil {
			pv.IInLimitDAC_mA = &v
		}
	}
	if v, err := a.dev.DieMilliC(); err == nil {
		pv.Die_mC = &v
	}
	out = append(out, halcore.Reading{Kind: "power", Payload: pv})

	// Charger summary + raw bitfields
	sum, _ := a.dev.Summary()
	ss, cs, st, _ := a.dev.RawStatus()
	cv := types.ChargerValue{
		Phase:        phaseFrom(sum),
		InputLimited: types.ChargerInputLimited{VinUvcl: sum.VinUvcl, IInLimit: sum.IinLimit},
		OKToCharge:   sum.OkToCharge,
		Faults:       types.ChargerFaults{BatMissing: sum.BatMissing, BatShort: sum.BatShort, ThermalShutdown: sum.ThermalShutdown},
		Raw:          types.ChargerRaw{SystemStatus: ss, ChargerState: cs, ChargeStatus: st},
		TS:           nowt,
	}
	out = append(out, halcore.Reading{Kind: "charger", Payload: cv})

	return out, nil
}

func parseChemistry(s string) types.Chemistry {
	switch s {
	case "lithium":
		return types.ChemLithium
	case "lead_acid":
		return types.ChemLeadAcid
	default:
		return types.ChemUnknown
	}
}

func phaseFrom(s StatusSummary) types.ChargerPhase {
	switch {
	case s.BatMissing || s.BatShort || s.ThermalShutdown:
		return types.PhaseFault
	case s.Equalize:
		return types.PhaseEqualize
	case s.Absorb:
		return types.PhaseAbsorb
	case s.Precharge:
		return types.PhasePrecharge
	case s.InCCCV:
		if s.CC {
			return types.PhaseCC
		}
		if s.CV {
			return types.PhaseCV
		}
		return types.PhaseCC
	case s.Suspended:
		return types.PhaseSuspended
	default:
		return types.PhaseIdle
	}
}

// Controls (device-agnostic).
func (a *adaptor) Control(kind, method string, payload any) (any, error) {
	if kind != "charger" {
		return nil, halcore.ErrUnsupported
	}
	switch method {
	case "set_input_current_limit":
		if p, ok := payload.(types.LTC4015SetInputCurrentLimit); ok {
			return okReply(a.dev.SetIinLimit_mA(int32(p.MA)))
		}
		return nil, halerr.ErrInvalidPayload
	case "set_charge_current":
		if p, ok := payload.(types.LTC4015SetChargeCurrent); ok {
			return okReply(a.dev.SetIChargeTarget_mA(int32(p.MA)))
		}
		return nil, halerr.ErrInvalidPayload
	case "set_vin_uvcl":
		if p, ok := payload.(types.LTC4015SetVinUVCL); ok {
			return okReply(a.dev.SetVinUvcl_mV(int32(p.MV)))
		}
		return nil, halerr.ErrInvalidPayload
	case "apply_profile":
		if p, ok := payload.(types.LTC4015ApplyProfile); ok {
			return a.applyProfileTyped(p)
		}
		return nil, halerr.ErrInvalidPayload
	case "read_alerts":
		if _, ok := payload.(types.LTC4015ReadAlerts); ok {
			ev, err := a.dev.DrainAlerts()
			if err != nil {
				return nil, err
			}
			return types.LTC4015ReadAlertsReply{Limit: ev.Limit, ChgState: ev.ChgState, ChgStatus: ev.ChgStatus}, nil
		}
		return nil, halerr.ErrInvalidPayload
	default:
		return nil, halcore.ErrUnsupported
	}
}

func (a *adaptor) applyProfileTyped(p types.LTC4015ApplyProfile) (types.LTC4015ApplyProfileReply, error) {
	if p.Cells != nil && *p.Cells > 0 {
		_ = a.dev.Configure(configLite{Cells: uint8(*p.Cells)})
	}

	var chem string
	switch p.Chemistry {
	case types.ChemLeadAcid:
		chem = "lead_acid"
	case types.ChemLithium:
		chem = "lithium"
	default:
		chem = "unknown"
	}
	_ = a.dev.SetSuspend(true)
	defer a.dev.SetSuspend(false)

	switch chem {
	case "lead_acid":
		if la, ok := a.dev.LeadAcid(); ok {
			if p.VCharge_mVPerCell != nil {
				if err := la.SetVChargeSetting_mVPerCell(*p.VCharge_mVPerCell, p.EnableTempComp != nil && *p.EnableTempComp); err != nil {
					return types.LTC4015ApplyProfileReply{}, err
				}
			}
			if p.VAbsorbDelta_mV != nil {
				if err := la.SetVAbsorbDelta_mVPerCell(*p.VAbsorbDelta_mV); err != nil {
					return types.LTC4015ApplyProfileReply{}, err
				}
			}
			if p.VEqualizeDelta_mV != nil {
				if err := la.SetVEqualizeDelta_mVPerCell(*p.VEqualizeDelta_mV); err != nil {
					return types.LTC4015ApplyProfileReply{}, err
				}
			}
			if p.MaxAbsorbTime_s != nil {
				if err := la.SetMaxAbsorbTime_s(*p.MaxAbsorbTime_s); err != nil {
					return types.LTC4015ApplyProfileReply{}, err
				}
			}
			if p.EqualizeTime_s != nil {
				if err := la.SetEqualizeTime_s(*p.EqualizeTime_s); err != nil {
					return types.LTC4015ApplyProfileReply{}, err
				}
			}
			if p.EnableTempComp != nil {
				if err := la.EnableLeadAcidTempComp(*p.EnableTempComp); err != nil {
					return types.LTC4015ApplyProfileReply{}, err
				}
			}
		}
	case "lithium":
		if p.VCharge_mVPerCell == nil { // keep example simple; map lithium to charge current if provided
			// no-op
		}
		// Optional: extend with more lithium profile fields later.
	}

	ss, cs, st, _ := a.dev.RawStatus()
	return types.LTC4015ApplyProfileReply{OK: true, Raw: types.ChargerRaw{SystemStatus: ss, ChargerState: cs, ChargeStatus: st}}, nil
}

// Small helpers.
func okReply(err error) (struct{ OK bool }, error) {
	if err != nil {
		return struct{ OK bool }{}, err
	}
	return struct{ OK bool }{OK: true}, nil
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

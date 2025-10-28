package types

// ------------------------
// Battery / Charger (ltc4015)
// ------------------------

type BatteryInfo struct {
	Cells      uint8  `json:"cells"`
	Chem       string `json:"chem"`       // "li" | "leadacid" | "auto"
	RSNSB_uOhm uint32 `json:"rsnsb_uohm"` // battery sense
	Bus        string `json:"bus"`
	Addr       uint16 `json:"addr"`
}

// Retained value: hal/cap/power/battery/<name>/value
type BatteryValue struct {
	PackMilliV      int32  `json:"pack_mV"`
	PerCellMilliV   int32  `json:"per_cell_mV"`
	IBatMilliA      int32  `json:"ibat_mA"`
	TempMilliC      int32  `json:"temp_mC"`
	BSR_uOhmPerCell uint32 `json:"bsr_uohm_per_cell"`
}

type ChargerInfo struct {
	RSNSI_uOhm uint32 `json:"rsnsi_uohm"`
	Bus        string `json:"bus"`
	Addr       uint16 `json:"addr"`
}

// Retained value: hal/cap/power/charger/<name>/value
type ChargerValue struct {
	VIN_mV  int32  `json:"vin_mV"`
	VSYS_mV int32  `json:"vsys_mV"`
	IIn_mA  int32  `json:"iin_mA"`
	State   uint16 `json:"state"`  // raw CHARGER_STATE bits
	Status  uint16 `json:"status"` // raw CHARGE_STATUS bits
	Sys     uint16 `json:"sys"`    // raw SYSTEM_STATUS bits
}

// Controls
type ChargerEnable struct{ On bool }           // verb: "enable"
type SetInputLimit struct{ MilliA int32 }      // verb: "set_input_limit"
type SetChargeTarget struct{ MilliA int32 }    // verb: "set_charge_target"
type SetVinWindow struct{ Lo_mV, Hi_mV int32 } // verb: "set_vin_window"

// ChargerConfigure is a partial update. Nil means "leave as-is".
type ChargerConfigure struct {
	// Global behaviour
	Enable           *bool `json:"enable,omitempty"`              // true => resume, false => suspend
	LeadAcidTempComp *bool `json:"lead_acid_temp_comp,omitempty"` // true => enable LA temp comp, false => disable LA temp comp

	CfgSet   *uint16 `json:"cfg_set,omitempty"`   // ltc4015.ConfigBits mask to SET
	CfgClear *uint16 `json:"cfg_clear,omitempty"` // ltc4015.ConfigBits mask to CLEAR

	// Targets and limits (driver enforces RSNS requirements)
	IinLimit_mA         *int32  `json:"iin_limit_mA,omitempty"`
	IChargeTarget_mA    *int32  `json:"icharge_target_mA,omitempty"`
	IinHigh_mA          *int32  `json:"iin_high_mA,omitempty"`
	IbatLow_mA          *int32  `json:"ibat_low_mA,omitempty"`
	DieTempHigh_mC      *int32  `json:"die_temp_high_mC,omitempty"`
	BSRHigh_uOhmPerCell *uint32 `json:"bsr_high_uohm_per_cell,omitempty"`

	// Windows (0/0 is permitted; driver writes codes as given)
	VinLo_mV         *int32  `json:"vin_lo_mV,omitempty"`
	VinHi_mV         *int32  `json:"vin_hi_mV,omitempty"`
	VsysLo_mV        *int32  `json:"vsys_lo_mV,omitempty"`
	VsysHi_mV        *int32  `json:"vsys_hi_mV,omitempty"`
	VbatLo_mVPerCell *int32  `json:"vbat_lo_mV_per_cell,omitempty"`
	VbatHi_mVPerCell *int32  `json:"vbat_hi_mV_per_cell,omitempty"`
	NTCRatioHi       *uint16 `json:"ntc_ratio_hi,omitempty"`
	NTCRatioLo       *uint16 `json:"ntc_ratio_lo,omitempty"`
	VinUVCL_mV       *int32  `json:"vin_uvcl_mV,omitempty"`

	// Optional explicit alert masks (advanced)
	AlertMask *ChargerAlertMask `json:"alert_mask,omitempty"`
}

type ChargerAlertMask struct {
	Limit     *uint16 `json:"limit,omitempty"`      // ltc4015.LimitEnable
	ChgState  *uint16 `json:"chg_state,omitempty"`  // ltc4015.ChargerStateEnable
	ChgStatus *uint16 `json:"chg_status,omitempty"` // ltc4015.ChargeStatusEnable
}

// ------------ Small payloads for verbs ------------

type VinWindowSet struct{ Lo_mV, Hi_mV int32 }
type VbatWindowSet struct{ Lo_mVPerCell, Hi_mVPerCell int32 }
type VsysWindowSet struct{ Lo_mV, Hi_mV int32 }
type CurrentMA struct{ MilliA int32 }
type VoltageMV struct{ MilliV int32 }
type TempMilliC struct{ MilliC int32 }
type ResistanceMicroOhmPerCell struct{ MicroOhmPerCell uint32 }
type NTCRatioWindowRaw struct{ Hi, Lo uint16 }

type ChargerConfigBitsUpdate struct {
	Set, Clear uint16
}

// SYSTEM_STATUS (0x39)
type SystemStatus uint16

const (
	ChargerEnabled  SystemStatus = 1 << 13
	MpptEnPin       SystemStatus = 1 << 11
	EqualizeReq     SystemStatus = 1 << 10
	DrvccGood       SystemStatus = 1 << 9
	CellCountError  SystemStatus = 1 << 8
	OkToCharge      SystemStatus = 1 << 6
	NoRt            SystemStatus = 1 << 5
	ThermalShutdown SystemStatus = 1 << 4
	VinOvlo         SystemStatus = 1 << 3
	VinGtVbat       SystemStatus = 1 << 2
	IntvccGt4p3V    SystemStatus = 1 << 1
	IntvccGt2p8V    SystemStatus = 1 << 0
)

// CHARGER_STATE_ALERTS (0x37)
type ChargerStateBits uint16

const (
	EqualizeCharge     ChargerStateBits = 1 << 10
	AbsorbCharge       ChargerStateBits = 1 << 9
	ChargerSuspended   ChargerStateBits = 1 << 8
	Precharge          ChargerStateBits = 1 << 7
	CCCVCharge         ChargerStateBits = 1 << 6
	NTCPause           ChargerStateBits = 1 << 5
	TimerTerm          ChargerStateBits = 1 << 4
	COverXTerm         ChargerStateBits = 1 << 3
	MaxChargeTimeFault ChargerStateBits = 1 << 2
	BatMissingFault    ChargerStateBits = 1 << 1
	BatShortFault      ChargerStateBits = 1 << 0
)

// CHARGE_STATUS (0x38)
type ChargeStatusBits uint16

const (
	VinUvclActive  ChargeStatusBits = 1 << 3
	IinLimitActive ChargeStatusBits = 1 << 2
	ConstCurrent   ChargeStatusBits = 1 << 1
	ConstVoltage   ChargeStatusBits = 1 << 0
)

// Generic pairing of a bit value with a printable name.
// T is a uint16-like type (e.g., SystemStatus, ChargerStateBits, ChargeStatusBits).
type BitName[T ~uint16] struct {
	Bit  T
	Name string
}

// BitIter is a zero-alloc iterator over set bits in a value, filtered by a table.
// Caller advances with Next(); no callbacks, no closures.
type BitIter[T ~uint16] struct {
	v     uint16
	i     int
	table []BitName[T]
}

// NewBitIter constructs an iterator over set bits present in v that also exist in table.
func NewBitIter[T ~uint16](v T, table []BitName[T]) BitIter[T] {
	return BitIter[T]{v: uint16(v), i: 0, table: table}
}

// Next returns the next SET bit: (name, ok). ok=false when done.
func (it *BitIter[T]) Next() (string, bool) {
	for it.i < len(it.table) {
		e := it.table[it.i]
		it.i++
		if (it.v & uint16(e.Bit)) != 0 {
			return e.Name, true
		}
	}
	return "", false
}

// Reset allows reusing the iterator.
func (it *BitIter[T]) Reset() { it.i = 0 }

// NextAny returns the next table entry: (name, set, ok).
// set indicates whether the bit is present in the value.
func (it *BitIter[T]) NextAny() (string, bool, bool) {
	if it.i >= len(it.table) {
		return "", false, false
	}
	e := it.table[it.i]
	it.i++
	set := (it.v & uint16(e.Bit)) != 0
	return e.Name, set, true
}

// -----------------------------
// Display tables for bitfields
// -----------------------------

// ChargerStateBits display (ordering is cosmetic).
var ChargerStateTable = [...]BitName[ChargerStateBits]{
	{BatShortFault, "bat_short"},
	{BatMissingFault, "bat_missing"},
	{MaxChargeTimeFault, "max_charge_time_fault"},
	{COverXTerm, "c_over_x_term"},
	{TimerTerm, "timer_term"},
	{NTCPause, "ntc_pause"},
	{Precharge, "precharge"},
	{CCCVCharge, "cccv"},
	{AbsorbCharge, "absorb"},
	{EqualizeCharge, "equalize"},
	{ChargerSuspended, "suspended"},
}

// ChargeStatusBits display.
var ChargeStatusTable = [...]BitName[ChargeStatusBits]{
	{IinLimitActive, "iin_limited"},
	{VinUvclActive, "uvcl_active"},
	{ConstCurrent, "cc_phase"},
	{ConstVoltage, "cv_phase"},
}

// SystemStatus display.
var SystemStatusTable = [...]BitName[SystemStatus]{
	{ChargerEnabled, "charger_enabled"},
	{MpptEnPin, "mppt_en_pin"},
	{EqualizeReq, "equalize_req"},
	{DrvccGood, "drvcc_good"},
	{CellCountError, "cell_count_error"},
	{OkToCharge, "ok_to_charge"},
	{NoRt, "no_rt"},
	{ThermalShutdown, "thermal_shutdown"},
	{VinOvlo, "vin_ovlo"},
	{VinGtVbat, "vin_gt_vbat"},
	{IntvccGt4p3V, "intvcc_gt_4p3v"},
	{IntvccGt2p8V, "intvcc_gt_2p8v"},
}

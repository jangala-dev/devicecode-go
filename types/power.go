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
	Enable   *bool   `json:"enable,omitempty"`    // true => resume, false => suspend
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

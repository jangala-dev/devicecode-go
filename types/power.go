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

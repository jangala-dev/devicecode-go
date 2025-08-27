package main

import (
	"machine"
	"time"

	"devicecode-go/drivers/ltc4015"
)

// bit describes a single bit in a status/alert register.
type bit struct {
	pos   uint8
	label string
}

// ----- Bit maps (from datasheet) -----

// SYSTEM_STATUS (0x39) bits we care about (others are reserved/unused).
var systemStatusBits = []bit{
	{13, "charger_enabled"}, // actively charging
	{11, "mppt_en_pin"},     // MPPT pin high
	{10, "equalize_req"},    // equalize queued/running (lead-acid)
	{9, "drvcc_good"},
	{8, "cell_count_error"},
	{6, "ok_to_charge"},
	{5, "no_rt"},
	{4, "thermal_shutdown"},
	{3, "vin_ovlo"},
	{2, "vin_gt_vbat"},
	{1, "intvcc_gt_4p3v"},
	{0, "intvcc_gt_2p8v"},
}

// LIMIT_ALERTS (0x36)
var limitAlertBits = []bit{
	{15, "meas_sys_valid"},
	// 14 reserved
	{13, "qcount_lo"},
	{12, "qcount_hi"},
	{11, "vbat_lo"},
	{10, "vbat_hi"},
	{9, "vin_lo"},
	{8, "vin_hi"},
	{7, "vsys_lo"},
	{6, "vsys_hi"},
	{5, "iin_hi"},
	{4, "ibat_lo"},
	{3, "die_temp_hi"},
	{2, "bsr_hi"},
	{1, "ntc_ratio_hi(cold)"},
	{0, "ntc_ratio_lo(hot)"},
}

// CHARGER_STATE_ALERTS (0x37)
var chargerStateAlertBits = []bit{
	{10, "equalize_charge"},
	{9, "absorb_charge"},
	{8, "charger_suspended"},
	{7, "precharge"},
	{6, "cc_cv_charge"},
	{5, "ntc_pause"},
	{4, "timer_term"},
	{3, "c_over_x_term"},
	{2, "max_charge_time_fault"},
	{1, "bat_missing_fault"},
	{0, "bat_short_fault"},
}

// CHARGE_STATUS_ALERTS (0x38)
var chargeStatusAlertBits = []bit{
	{3, "vin_uvcl_active"},
	{2, "iin_limit_active"},
	{1, "constant_current"},
	{0, "constant_voltage"},
}

// printFlags prints a comma-separated list of set flags in v according to map bits.
// Uses print/println to avoid fmt overhead. Also includes any unknown set bits.
func printFlags(title string, v uint16, bits []bit) {
	print(title, " (", v, "): ")
	count := 0
	for _, b := range bits {
		if (v>>b.pos)&1 == 1 {
			if count > 0 {
				print(", ")
			}
			print(b.label)
			count++
		}
	}
	// report unexpected/reserved bits if they are set
	var knownMask uint16
	for _, b := range bits {
		knownMask |= 1 << b.pos
	}
	extra := v &^ knownMask
	if extra != 0 {
		if count > 0 {
			print(", ")
		}
		print("unknown_bits:")
		first := true
		for i := uint8(0); i < 16; i++ {
			if (extra>>i)&1 == 1 {
				if !first {
					print("|")
				}
				print("b", int(i))
				first = false
			}
		}
		count++
	}
	if count == 0 {
		print("none")
	}
	println()
}

func main() {
	time.Sleep(2 * time.Second)
	machine.Serial.Configure(machine.UARTConfig{})

	println("LTC4015 Pico test starting...")

	// Configure I2C0 (Pico default).
	machine.I2C0.Configure(machine.I2CConfig{
		Frequency: 400 * machine.KHz,
		SDA:       machine.I2C0_SDA_PIN,
		SCL:       machine.I2C0_SCL_PIN,
	})

	cfg := ltc4015.Config{
		RSNSB_uOhm: 3330, // 0.00333Ω
		RSNSI_uOhm: 1670, // 0.00167Ω
		Cells:      6,
		Chem:       ltc4015.ChemLeadAcid,
	}

	dev := ltc4015.New(machine.I2C0, cfg)
	if err := dev.Configure(cfg); err != nil {
		println("configure error")
		for {
			time.Sleep(time.Hour)
		}
	}

	println("LTC4015 Lead-Acid 6-cell test starting")

	// Enable Coulomb counter and keep measurement system on (typed config bits).
	_ = dev.SetConfigBits(ltc4015.CfgEnableQCount | ltc4015.CfgForceMeasSysOn)
	// Example: trigger a BSR run (self-clearing bit in hardware).
	// _ = dev.SetConfigBits(ltc4015.CfgRunBSR)

	// Enable all documented LIMIT alerts (excludes reserved bit 14)
	_ = dev.EnableLimitAlertsMask(
		ltc4015.LaMeasSysValid |
			ltc4015.LaQCountLo | ltc4015.LaQCountHi |
			ltc4015.LaVBATLo | ltc4015.LaVBATHi |
			ltc4015.LaVINLo | ltc4015.LaVINHi |
			ltc4015.LaVSYSLo | ltc4015.LaVSYSHi |
			ltc4015.LaIINHi | ltc4015.LaIBATLo |
			ltc4015.LaDieTempHi | ltc4015.LaBSRHi |
			ltc4015.LaNTCRatioHi | ltc4015.LaNTCRatioLo,
	)

	// Enable all documented CHARGER_STATE alerts
	_ = dev.EnableChargerStateAlertsMask(
		ltc4015.CsEqualizeCharge | ltc4015.CsAbsorbCharge |
			ltc4015.CsChargerSuspended | ltc4015.CsPrecharge |
			ltc4015.CsCCCVCharge | ltc4015.CsNTCPause |
			ltc4015.CsTimerTerm | ltc4015.CsCOverXTerm |
			ltc4015.CsMaxChargeTimeFault | ltc4015.CsBatMissingFault |
			ltc4015.CsBatShortFault,
	)

	// Enable all documented CHARGE_STATUS alerts
	_ = dev.EnableChargeStatusAlertsMask(
		ltc4015.CsVinUvclActive | ltc4015.CsIinLimitActive |
			ltc4015.CsConstCurrent | ltc4015.CsConstVoltage,
	)

	for {
		valid, err := dev.MeasSystemValid()
		if err != nil {
			println("MEAS_SYS_VALID error")
			time.Sleep(time.Second)
			continue
		}
		if !valid {
			println("Measurement system not valid yet")
			time.Sleep(time.Second)
			continue
		}

		vcell, _ := dev.BatteryMilliVPerCell()
		vpack, _ := dev.BatteryMilliVPack()
		vin, _ := dev.VinMilliV()
		vsys, _ := dev.VsysMilliV()
		ibat, _ := dev.IbatMilliA()
		iin, _ := dev.IinMilliA()
		temp, _ := dev.DieMilliC()
		bsr, _ := dev.BSRMicroOhmPerCell()
		qc, _ := dev.QCount()

		sys, _ := dev.SystemStatus()
		limAlerts, _ := dev.ReadLimitAlerts()
		chrStAlerts, _ := dev.ReadChargerStateAlerts()
		chrgAlerts, _ := dev.ReadChargeStatusAlerts()

		println("------------------------------------------")
		println("VBAT per cell (mV):", vcell)
		println("VBAT pack (mV):", vpack)
		println("VIN (mV):", vin)
		println("VSYS (mV):", vsys)
		println("IBAT (mA):", ibat)
		println("IIN (mA):", iin)
		println("Die Temp (mC):", temp)
		println("BSR/cell (µΩ):", bsr)
		println("QCOUNT:", qc)

		// Decoded registers (cast typed masks to uint16 for printFlags)
		printFlags("SystemStatus", uint16(sys), systemStatusBits)
		printFlags("Limit Alerts", uint16(limAlerts), limitAlertBits)
		printFlags("Charger State Alerts", uint16(chrStAlerts), chargerStateAlertBits)
		printFlags("Charge Status Alerts", uint16(chrgAlerts), chargeStatusAlertBits)

		// Clear alert latches after reporting
		_ = dev.ClearLimitAlerts()
		_ = dev.ClearChargerStateAlerts()
		_ = dev.ClearChargeStatusAlerts()

		time.Sleep(2 * time.Second)
	}
}

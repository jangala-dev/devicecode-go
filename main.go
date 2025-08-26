package main

import (
	"machine"
	"time"

	"devicecode-go/drivers/ltc4015"
)

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

	// Explicitly enable Coulomb counter
	_ = dev.EnableQCount(true)

	// Optionally force measurement system on without VIN
	_ = dev.ForceMeasSystemOn(true)

	_ = dev.EnableLimitAlerts(0xFFFF)
	_ = dev.EnableChargerStateAlerts(0xFFFF)
	_ = dev.EnableChargeStatusAlerts(0xFFFF)

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
		println("Die Temp (mC):", temp) // milli-degrees Celsius
		println("BSR/cell (µΩ):", bsr)
		println("QCOUNT:", qc)

		println("SystemStatus reg:", sys)
		println("Limit Alerts reg:", limAlerts)
		println("ChargerStateAlerts reg:", chrStAlerts)
		println("ChargeStatusAlerts reg:", chrgAlerts)

		_ = dev.ClearLimitAlerts()
		_ = dev.ClearChargerStateAlerts()
		_ = dev.ClearChargeStatusAlerts()

		time.Sleep(2 * time.Second)
	}
}

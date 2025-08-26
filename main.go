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

	i2c := machine.I2C0
	err := i2c.Configure(machine.I2CConfig{
		SDA:       machine.I2C0_SDA_PIN,
		SCL:       machine.I2C0_SCL_PIN,
		Frequency: 400 * machine.KHz, // safe default
	})
	if err != nil {
		println("could not configure i2c:", err)
		return
	}

	// Create device instance
	dev := ltc4015.New(machine.I2C0)

	// Apply configuration (example: lithium battery, 2 cells, 10 mΩ sense resistors)
	cfg := ltc4015.Config{
		RSNSB:    0.00333, // 10 mΩ battery sense resistor
		RSNSI:    0.00167, // 10 mΩ input sense resistor
		Lithium:  false,
		Cells:    6,
		Features: 0, // no extra features enabled yet
		RNTC:     10000.0,
		RBSRT:    100000.0,
		RBSRB:    10000.0,
		RVIN1:    100000.0,
		RVIN2:    10000.0,
		Address:  0x68,
	}
	if err := dev.Configure(cfg); err != nil {
		println("failed to configure LTC4015:", err.Error())
		return
	}

	for {
		// Read supply voltages
		vin, _ := dev.ReadVIN()   // mV
		vsys, _ := dev.ReadVSYS() // mV
		vbat, _ := dev.ReadVBAT() // mV

		// Read currents
		iin, _ := dev.ReadIIN()   // mA
		ibat, _ := dev.ReadIBAT() // mA

		// Read temperature
		temp, _ := dev.ReadDieTemp() // tenths of °C

		print("VIN=", vin, " mV\n")
		print("IIN=", iin, " mA\n")
		print("VSYS=", vsys, " mV\n")
		print("Temp=", temp, " (tenths °C)\n")

		batt_missing, _ := dev.StateBatteryMissing()
		println("batt_missing: ", batt_missing)
		batt_shorted, _ := dev.StateBatteryShort()

		if !batt_missing && !batt_shorted {
			print("VBAT=", vbat, " mV\n")
			print("IBAT=", ibat, " mA\n")
		}

		print("\n")

		time.Sleep(time.Second)
	}
}

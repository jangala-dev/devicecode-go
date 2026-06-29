//go:build (rp2040 || rp2350) && pico_cm5_emulator

package setups

import (
	serialraw "devicecode-go/services/hal/devices/serial_raw"
	"devicecode-go/types"
)

var SelectedPlan = ResourcePlan{
	UART: []UARTPlan{
		// Pico 1 CM5 emulator link UART.
		// Wire Pico 1 GP0/TX -> Pico 2 GP5/RX and Pico 1 GP1/RX <- Pico 2 GP4/TX.
		{ID: "uart0", TX: 0, RX: 1, Baud: 115_200},
	},
}

// Keep the emulator link at the default diagnostic raw-serial RX size so it
// matches the hardware build while testing the line-reader/copy optimisations.
const (
	fabricRawSerialRXSize = 512
	fabricRawSerialTXSize = 512
)

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		{ID: "uart0_raw", Type: "serial_raw", Params: serialraw.Params{
			Bus:    "uart0",
			Domain: "io",
			Name:   "uart0",
			Baud:   115_200,
			RXSize: fabricRawSerialRXSize,
			TXSize: fabricRawSerialTXSize,
		}},
	},
}

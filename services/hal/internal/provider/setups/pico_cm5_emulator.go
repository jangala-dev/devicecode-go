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

// Keep the emulator link under the same bounded serial-session constraint as
// the MCU Fabric link; the emulator must stream and apply flow control rather
// than buffering whole Fabric frames in the HAL session.
const rawSerialSessionSize = 512

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		{ID: "uart0_raw", Type: "serial_raw", Params: serialraw.Params{
			Bus:    "uart0",
			Domain: "io",
			Name:   "uart0",
			Baud:   115_200,
			RXSize: rawSerialSessionSize,
			TXSize: rawSerialSessionSize,
		}},
	},
}

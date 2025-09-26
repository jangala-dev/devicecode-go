//go:build pico && pico_bb_proto_1

package setups

import (
	_ "devicecode-go/services/hal/devices/aht20"
	_ "devicecode-go/services/hal/devices/led"
	_ "devicecode-go/services/hal/devices/shtc3"

	"devicecode-go/types"
)

// SelectedPlan wires controllers to pins and sets operating parameters for this setup.
var SelectedPlan = ResourcePlan{
	I2C: []I2CPlan{
		{ID: "i2c0", SDA: 4, SCL: 5, Hz: 400_000},
		{ID: "i2c1", SDA: 2, SCL: 3, Hz: 400_000},
	},
	UART: []UARTPlan{
		{ID: "uart0", TX: 0, RX: 1, Baud: 115200},
		// add more as needed
	},
}

// SelectedSetup lists logical devices for HAL to instantiate on boot.
// Names are chosen for meaningful public addresses under hal/cap/…
// (for now, these enables are gpio_led; you can migrate to a switch kind later).
var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		// Gates / enables (published now under hal/cap/io/led/<name>/…)
		{ID: "button-led", Type: "gpio_led", Params: types.LEDParams{Pin: 11, Initial: true}}, // active-low ext. pull-up
		{ID: "eg25", Type: "gpio_led", Params: types.LEDParams{Pin: 6, Initial: false}},
		{ID: "rm520n", Type: "gpio_led", Params: types.LEDParams{Pin: 7, Initial: false}},
		{ID: "aw7915", Type: "gpio_led", Params: types.LEDParams{Pin: 8, Initial: false}},
		{ID: "cm5-5v", Type: "gpio_led", Params: types.LEDParams{Pin: 9, Initial: false}},
		{ID: "fan-5v", Type: "gpio_led", Params: types.LEDParams{Pin: 10, Initial: false}},
		{ID: "boost-load", Type: "gpio_led", Params: types.LEDParams{Pin: 14, Initial: false}},

		// On-board LED
		{ID: "onboard", Type: "gpio_led", Params: types.LEDParams{Pin: 25, Initial: false}},

		// (future) I²C/SPI/UART devices when providers/devices are present.
	},
}

//go:build pico && pico_bb_proto_1

package setups

import (
	aht20dev "devicecode-go/services/hal/devices/aht20"
	"devicecode-go/services/hal/devices/gpio_dout"

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
		// Gates / enables -> switches (power domain)
		{ID: "mpcie-usb", Type: "gpio_switch", Params: gpio_dout.Params{Pin: 6, ActiveLow: false, Initial: false}},
		{ID: "m2", Type: "gpio_switch", Params: gpio_dout.Params{Pin: 7, ActiveLow: false, Initial: false}},
		{ID: "mpcie", Type: "gpio_switch", Params: gpio_dout.Params{Pin: 8, ActiveLow: false, Initial: false}},
		{ID: "cm5-5v", Type: "gpio_switch", Params: gpio_dout.Params{Pin: 9, ActiveLow: false, Initial: false}},
		{ID: "fan-5v", Type: "gpio_switch", Params: gpio_dout.Params{Pin: 10, ActiveLow: false, Initial: false}},
		{ID: "boost-load", Type: "gpio_switch", Params: gpio_dout.Params{Pin: 14, ActiveLow: false, Initial: false}},

		{ID: "onboard", Type: "gpio_led", Params: gpio_dout.Params{Pin: 25, ActiveLow: false, Initial: false}},

		{ID: "core", Type: "aht20", Params: aht20dev.Params{Bus: "i2c0"}},
	},
}

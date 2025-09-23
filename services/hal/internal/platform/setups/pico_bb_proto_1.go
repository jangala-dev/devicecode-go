//go:build pico && pico_bb_proto_1

package setups

import "devicecode-go/types"

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		// Gate / enables
		{ID: "btn_led_gate", Type: "gpio_led", Params: types.LEDParams{Pin: 11, Initial: true}}, // active-low ext. pull-up
		{ID: "buck_eg25_en", Type: "gpio_led", Params: types.LEDParams{Pin: 6, Initial: false}},
		{ID: "buck_rm520n_en", Type: "gpio_led", Params: types.LEDParams{Pin: 7, Initial: false}},
		{ID: "buck_aw7915_en", Type: "gpio_led", Params: types.LEDParams{Pin: 8, Initial: false}},
		{ID: "buck_cm5_en", Type: "gpio_led", Params: types.LEDParams{Pin: 9, Initial: false}},
		{ID: "buck_fan_en", Type: "gpio_led", Params: types.LEDParams{Pin: 10, Initial: false}},
		{ID: "boost_load_sw", Type: "gpio_led", Params: types.LEDParams{Pin: 14, Initial: false}},

		// On-board LED convenience
		{ID: "led0", Type: "gpio_led", Params: types.LEDParams{Pin: 25, Initial: false}},

		// --- Future when providers/devices are present ---
		// { ID:"ltc0",  Type:"ltc4015", Params: { Bus:"i2c1", Addr:0x67, SMBAlert:20, ... } },
		// { ID:"sens0", Type:"aht20",   Params: { Bus:"i2c0", Addr:0x38 } },
		// { ID:"serial0", Type:"uart",  Params: { Bus:"uart0", Baud:115200, Mode:"bytes" } },
		// { ID:"serial1", Type:"uart",  Params: { Bus:"uart1", Baud:115200, Mode:"bytes" } },
	},
}

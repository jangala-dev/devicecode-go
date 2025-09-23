//go:build pico && pico_rich_dev

package setups

import "devicecode-go/types"

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		{ID: "led0", Type: "gpio_led", Params: types.LEDParams{Pin: 25, Initial: false}},
	},
}

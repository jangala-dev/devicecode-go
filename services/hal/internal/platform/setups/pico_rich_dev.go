//go:build pico && pico_rich_dev

package setups

import "devicecode-go/types"

var SelectedPlan = ResourcePlan{
	I2C:  []I2CPlan{{ID: "i2c0", SDA: 4, SCL: 5, Hz: 400_000}},
	UART: nil,
}

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		{ID: "led0", Type: "gpio_led", Params: types.LEDParams{Pin: 25, Initial: false}},
	},
}

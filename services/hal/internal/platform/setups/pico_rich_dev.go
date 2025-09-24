//go:build pico && pico_rich_dev

package setups

import (
	_ "devicecode-go/services/hal/devices/led"
	shtc3dev "devicecode-go/services/hal/devices/shtc3"

	"devicecode-go/types"
)

var SelectedPlan = ResourcePlan{
	I2C:  []I2CPlan{{ID: "i2c0", SDA: 4, SCL: 5, Hz: 400_000}},
	UART: nil,
}

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		// On-board LED
		{ID: "led0", Type: "gpio_led", Params: types.LEDParams{Pin: 25, Initial: false}},

		// Environmental sensor on i2c0 (SHTC3 at fixed address 0x70)
		{ID: "sht0", Type: "shtc3", Params: shtc3dev.Params{Bus: "i2c0"}},
	},
}

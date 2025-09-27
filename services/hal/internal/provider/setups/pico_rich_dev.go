//go:build pico && pico_rich_dev

package setups

import (
	"devicecode-go/services/hal/devices/pwm_out"
	shtc3dev "devicecode-go/services/hal/devices/shtc3"
	"devicecode-go/types"
)

var SelectedPlan = ResourcePlan{
	I2C:  []I2CPlan{{ID: "i2c0", SDA: 4, SCL: 5, Hz: 400_000}},
	UART: nil,
}

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		// On-board LED (name => public address hal/cap/io/led/onboard/…)
		{ID: "onboard_led", Type: "pwm_out", Params: pwm_out.Params{
			Pin:    25,
			FreqHz: 1000,
			Top:    4095,
			Domain: "io",
			Name:   "onboard",
		}},

		// Environmental sensor on i2c0 (public addresses under hal/cap/env/*/core/…)
		{ID: "core", Type: "shtc3", Params: shtc3dev.Params{Bus: "i2c0"}},
	},
}

//go:build pico && pico_rich_dev

package setups

import (
	"devicecode-go/services/hal/devices/gpio_dout"
	ltc4015dev "devicecode-go/services/hal/devices/ltc4015"
	"devicecode-go/services/hal/devices/pwm_out"
	serialraw "devicecode-go/services/hal/devices/serial_raw"
	shtc3dev "devicecode-go/services/hal/devices/shtc3"
	"devicecode-go/types"
)

var SelectedPlan = ResourcePlan{
	I2C: []I2CPlan{
		{ID: "i2c0", SDA: 12, SCL: 13, Hz: 400_000},
		{ID: "i2c1", SDA: 18, SCL: 19, Hz: 400_000},
	},
	UART: []UARTPlan{
		// RP2040 default pins for Pico
		{ID: "uart0", TX: 0, RX: 1, Baud: 115_200},
		{ID: "uart1", TX: 4, RX: 5, Baud: 115_200},
	},
}

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{
		// On-board LED (name => public address hal/cap/io/led/onboard/…)
		{ID: "button_led", Type: "pwm_out", Params: pwm_out.Params{
			Pin:       11,
			FreqHz:    1000,
			Top:       4095,
			ActiveLow: true,
			Initial:   4095,
			Domain:    "io",
			Name:      "button-led",
		}},

		// Environmental sensor on i2c0 (public addresses under hal/cap/env/*/core/…)
		{ID: "core", Type: "shtc3", Params: shtc3dev.Params{Bus: "i2c0", Domain: "env", Name: "core"}},

		// Raw serial device bound to uart0 (public address hal/cap/io/serial/uart0/…)
		{ID: "uart0_raw", Type: "serial_raw", Params: serialraw.Params{
			Bus:    "uart0",
			Domain: "io",
			Name:   "uart0",
			Baud:   115_200,
			RXSize: 128,
			TXSize: 128,
		}},

		// Raw serial device bound to uart1 (public address hal/cap/io/serial/uart1/…)
		{ID: "uart1_raw", Type: "serial_raw", Params: serialraw.Params{
			Bus:    "uart1",
			Domain: "io",
			Name:   "uart1",
			Baud:   115_200,
			RXSize: 128,
			TXSize: 128,
		}},

		{ID: "charger0", Type: "ltc4015", Params: ltc4015dev.Params{
			Bus: "i2c1", Addr: 0, RSNSB_uOhm: 3_330, RSNSI_uOhm: 1_670, Cells: 6,
			Chem: "la", SMBAlertPin: 20, VinLo_mV: 9_000, VinHi_mV: 11_000,
			BSRHi_uOhmPerCell: 100_000,
			DomainBattery:     "power", DomainCharger: "power", Name: "internal",
		}},

		// Gates / enables -> switches (power domain)
		{ID: "mpcie-usb", Type: "gpio_switch", Params: gpio_dout.Params{
			Pin: 6, ActiveLow: false, Initial: false,
			Domain: "power", Name: "mpcie-usb",
		}},
		{ID: "m2", Type: "gpio_switch", Params: gpio_dout.Params{
			Pin: 7, ActiveLow: false, Initial: false,
			Domain: "power", Name: "m2",
		}},
		{ID: "mpcie", Type: "gpio_switch", Params: gpio_dout.Params{
			Pin: 8, ActiveLow: false, Initial: false,
			Domain: "power", Name: "mpcie",
		}},
		{ID: "cm5", Type: "gpio_switch", Params: gpio_dout.Params{
			Pin: 9, ActiveLow: false, Initial: false,
			Domain: "power", Name: "cm5",
		}},
		{ID: "fan", Type: "gpio_switch", Params: gpio_dout.Params{
			Pin: 10, ActiveLow: false, Initial: false,
			Domain: "power", Name: "fan",
		}},
		{ID: "boost-load", Type: "gpio_switch", Params: gpio_dout.Params{
			Pin: 14, ActiveLow: false, Initial: false,
			Domain: "power", Name: "boost-load",
		}},
	},

	// Declarative polling schedules applied by HAL after devices are registered.
	Pollers: []types.PollSpec{
		// Read the SHTC3 sensor periodically. Due to device-level dedup in HAL,
		// polling temperature suffices (humidity is emitted by the same read).
		{Domain: "env", Kind: "temperature", Name: "core", Verb: "read", IntervalMs: 1_000, JitterMs: 100},
		{Domain: "power", Kind: "battery", Name: "internal", Verb: "read", IntervalMs: 1_000, JitterMs: 100},
	},
}

//go:build pico && pico_rich_dev

package setups

import (
	"devicecode-go/services/hal/devices/gpio_dout"
	ltc4015dev "devicecode-go/services/hal/devices/ltc4015"
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

		{ID: "button_led", Type: "gpio_led", Params: gpio_dout.Params{
			Pin: 11, ActiveLow: false, Initial: true,
			Domain: "io", Name: "button_led",
		}},

		// Environmental sensor on i2c0 (public addresses under hal/cap/env/*/core/…)
		{ID: "core", Type: "shtc3", Params: shtc3dev.Params{Bus: "i2c0", Domain: "env", Name: "core"}},

		// Raw serial device bound to uart0 (public address hal/cap/io/serial/uart0/…)
		{ID: "uart0_raw", Type: "serial_raw", Params: serialraw.Params{
			Bus:    "uart0",
			Domain: "io",
			Name:   "uart0",
			Baud:   115_200,
			RXSize: 32,
			TXSize: 2048,
		}},

		// Raw serial device bound to uart1 (public address hal/cap/io/serial/uart1/…)
		{ID: "uart1_raw", Type: "serial_raw", Params: serialraw.Params{
			Bus:    "uart1",
			Domain: "io",
			Name:   "uart1",
			Baud:   115_200,
			RXSize: 32,
			TXSize: 512,
		}},

		{ID: "charger0", Type: "ltc4015", Params: ltc4015dev.Params{
			Bus: "i2c1", Addr: 0x68, SMBAlertPin: 20,
			RSNSB_uOhm: 3330, RSNSI_uOhm: 1670, Cells: 6,
			Chem:       "leadacid",
			NTCBiasOhm: 10000, R25Ohm: 10000, BetaK: 3435,
			QCountPrescale: 0,
			DomainBattery:  "power", DomainCharger: "power", Name: "internal",

			Boot: []types.BootAction{
				// {Verb: "disable"},
				{Verb: "configure", Payload: types.ChargerConfigure{
					VinLo_mV: PtrI32(9000), VinHi_mV: PtrI32(11000),
					BSRHigh_uOhmPerCell: PtrU32(100000), IChargeTarget_mA: PtrI32(2000),
					LeadAcidTempComp: PtrBool(false),
					// optional config-bit changes, limits, etc.
				}},
				{Verb: "enable"},
			},
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

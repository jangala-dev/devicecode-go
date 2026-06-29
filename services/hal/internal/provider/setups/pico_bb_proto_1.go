//go:build (rp2040 || rp2350) && pico_bb_proto_1

package setups

import (
	aht20dev "devicecode-go/services/hal/devices/aht20"
	"devicecode-go/services/hal/devices/gpio_dout"
	ltc4015dev "devicecode-go/services/hal/devices/ltc4015"
	"devicecode-go/services/hal/devices/pwm_out"
	"devicecode-go/services/hal/devices/rp2_temp"
	serialraw "devicecode-go/services/hal/devices/serial_raw"
	"devicecode-go/types"
)

var SelectedPlan = ResourcePlan{
	I2C: []I2CPlan{
		{ID: "i2c0", SDA: 12, SCL: 13, Hz: 100_000},
		{ID: "i2c1", SDA: 18, SCL: 19, Hz: 100_000},
	},
	UART: []UARTPlan{
		// RP2040 default pins for Pico
		{ID: "uart0", TX: 0, RX: 1, Baud: 115_200},
		{ID: "uart1", TX: 4, RX: 5, Baud: 115_200},
	},
}

// Keep raw serial rings at the default diagnostic size for this test build.
// This preserves serial_raw as the single UART abstraction while allowing the
// line-reader/copy optimisations to be tested without extra RX buffering.
const (
	rawSerialSessionSize  = 512
	fabricRawSerialRXSize = 512
	fabricRawSerialTXSize = 512
)

var SelectedSetup = types.HALConfig{
	Devices: []types.HALDevice{

		{ID: "button_led", Type: "pwm_out", Params: pwm_out.Params{
			Pin:       11,
			FreqHz:    1000,
			Top:       4095,
			ActiveLow: false,
			Initial:   4095,
			Domain:    "io",
			Name:      "button_led",
		}},

		// Environmental sensor on i2c0 (public addresses under hal/cap/env/*/core/…)
		{ID: "core", Type: "aht20", Params: aht20dev.Params{Bus: "i2c0", Domain: "env", Name: "core"}},

		{ID: "die_temp", Type: "rp2_temp", Params: rp2_temp.Params{Domain: "env", Name: "die"}},

		// Raw serial device bound to uart0 (public address hal/cap/io/serial/uart0/…)
		{ID: "uart0_raw", Type: "serial_raw", Params: serialraw.Params{
			Bus:    "uart0",
			Domain: "io",
			Name:   "uart0",
			Baud:   115_200,
			RXSize: fabricRawSerialRXSize,
			TXSize: fabricRawSerialTXSize,
		}},

		// Raw serial device bound to uart1 (public address hal/cap/io/serial/uart1/…)
		{ID: "uart1_raw", Type: "serial_raw", Params: serialraw.Params{
			Bus:    "uart1",
			Domain: "io",
			Name:   "uart1",
			Baud:   115_200,
			RXSize: rawSerialSessionSize,
			TXSize: rawSerialSessionSize,
		}},

		{ID: "charger0", Type: "ltc4015", Params: ltc4015dev.Params{
			Bus: "i2c1", Addr: 0x68, SMBAlertPin: 20,
			RSNSB_uOhm: 3330, RSNSI_uOhm: 3330, Cells: 6,
			Chem:       "leadacid",
			NTCBiasOhm: 10000, R25Ohm: 10000, BetaK: 3435,
			QCountPrescale: 0,
			DomainBattery:  "power", DomainCharger: "power", Name: "internal",
			BootDelayMs:              1_000,
			BootGapMs:                100,
			PostConfigureReadDelayMs: 250,

			Boot: []types.BootAction{
				{Verb: "configure", Payload: types.ChargerConfigure{
					VinLo_mV: PtrI32(9000), VinHi_mV: PtrI32(11000),
					BSRHigh_uOhmPerCell: PtrU32(100000), IChargeTarget_mA: PtrI32(9600),
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
		// Read the AHT20 sensor periodically. Due to device-level dedup in HAL,
		// polling temperature suffices (humidity is emitted by the same read).
		{Domain: "env", Kind: "temperature", Name: "core", Verb: "read", IntervalMs: 1_100, JitterMs: 100},
		{Domain: "power", Kind: "battery", Name: "internal", Verb: "read", IntervalMs: 1_300, JitterMs: 300},
		{Domain: "env", Kind: "temperature", Name: "die", Verb: "read", IntervalMs: 1_700, JitterMs: 100},
	},
}

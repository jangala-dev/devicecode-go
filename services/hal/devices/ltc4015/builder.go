package ltc4015dev

import (
	"context"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

// Params must be fully specified. No defaults are applied here.
type Params struct {
	// Wiring
	Bus         string // e.g. "i2c0" (required)
	Addr        uint16 // required
	SMBAlertPin int    // required (GPIO, active-low, open-drain)

	// Power-path characterisation
	RSNSB_uOhm uint32 // required (battery shunt)
	RSNSI_uOhm uint32 // required (input shunt)
	Cells      uint8  // required (series cell count)

	// Chemistry (explicit; no auto-detect in the HAL)
	// Accept "lithium", "lifepo", "leadacid".
	Chem string // required

	// Thermistor (explicit)
	NTCBiasOhm uint32 // required
	R25Ohm     uint32 // required
	BetaK      uint32 // required

	// Device features
	QCountPrescale uint16 // required (choose in setup; 0 => keep HW default)

	// Addressing
	DomainBattery string // required
	DomainCharger string // required
	Name          string // required

	// Boot actions are optional charger control writes applied after startup.
	// They are deliberately paced because LTC4015 I2C bursts at boot have
	// previously interfered with the UART0 Fabric path on real hardware.
	Boot []types.BootAction `json:"boot,omitempty"`

	// Optional pacing for Boot actions. Zero uses conservative defaults:
	// defer for 5s, leave 100ms between actions, and coalesce a single
	// sample 250ms after the last control write.
	BootDelayMs              uint32 `json:"boot_delay_ms,omitempty"`
	BootGapMs                uint32 `json:"boot_gap_ms,omitempty"`
	PostConfigureReadDelayMs uint32 `json:"post_configure_read_delay_ms,omitempty"`
}

// Builder registration (strict; no legacy shims).
func init() { core.RegisterBuilder("ltc4015", builder{}) }

type builder struct{}

func (builder) Build(ctx context.Context, in core.BuilderInput) (core.Device, error) {
	p, ok := in.Params.(Params)
	if !ok {
		if pp, ok2 := in.Params.(*Params); ok2 && pp != nil {
			p = *pp
		} else {
			return nil, errcode.InvalidParams
		}
	}

	// Hard validation: all fields must be provided.
	switch {
	case p.Bus == "", p.Addr == 0, p.SMBAlertPin < 0,
		p.RSNSB_uOhm == 0, p.RSNSI_uOhm == 0, p.Cells == 0,
		p.Chem == "", p.NTCBiasOhm == 0, p.R25Ohm == 0, p.BetaK == 0,
		p.DomainBattery == "", p.DomainCharger == "", p.Name == "":
		return nil, errcode.InvalidParams
	}

	// Claim I2C and SMBALERT#.
	i2c, err := in.Res.Reg.ClaimI2C(in.ID, core.ResourceID(p.Bus))
	if err != nil {
		return nil, err
	}
	ph, err := in.Res.Reg.ClaimPin(in.ID, p.SMBAlertPin, core.FuncGPIOIn)
	if err != nil {
		in.Res.Reg.ReleaseI2C(in.ID, core.ResourceID(p.Bus))
		return nil, err
	}
	gpio := ph.AsGPIO()
	_ = gpio.ConfigureInput(core.PullUp) // SMBALERT# is OD, active-low

	// Capability addresses.
	name := p.Name
	domBat := p.DomainBattery
	domChg := p.DomainCharger

	dev := &Device{
		id:   in.ID,
		aBat: core.CapAddr{Domain: domBat, Kind: types.KindBattery, Name: name},
		aChg: core.CapAddr{Domain: domChg, Kind: types.KindCharger, Name: name},
		aCfg: core.CapAddr{Domain: domChg, Kind: types.KindChargerConfig, Name: name},
		aTmp: core.CapAddr{Domain: domChg, Kind: types.KindTemperature, Name: name},

		res:  in.Res,
		i2c:  i2c,
		pin:  p.SMBAlertPin,
		gpio: gpio,

		params: p,
	}
	return dev, nil
}

package ltc4015dev

import (
	"context"

	"devicecode-go/errcode"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

// Params defines wiring and behaviour for one LTC4015 instance.
type Params struct {
	Bus         string // e.g. "i2c0" (required)
	Addr        uint16 // optional; default driver.AddressDefault
	RSNSB_uOhm  uint32 // required (battery path sense)
	RSNSI_uOhm  uint32 // required (input path sense)
	Cells       uint8  // 0 => detect via regChemCells
	Chem        string // "li" | "la" | "auto" | ""(=> "li")
	SMBAlertPin int    // required: GPIO for SMBALERT# (active-low, OD)

	// Required naming.
	DomainBattery string
	DomainCharger string
	Name          string

	// Optional alert thresholds to initialise.
	VinLo_mV          int32
	VinHi_mV          int32
	BSRHi_uOhmPerCell uint32 // e.g. 100_000 to catch open battery
	QCountPrescale    uint16 // optional; 0 => leave hardware default
	TargetsWritable   bool   // set false if fixed-chem variant

	NTCBiasOhm uint32 // top resistor; default 10_000
	R25Ohm     uint32 // thermistor @25C; default 10_000
	BetaK      uint32 // default 3435 (our part, 25/85C)

}

// Builder registration.
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
	if p.Bus == "" || p.RSNSB_uOhm == 0 || p.RSNSI_uOhm == 0 || p.SMBAlertPin < 0 {
		return nil, errcode.InvalidParams
	}
	if p.DomainBattery == "" || p.DomainCharger == "" || p.Name == "" {
		return nil, errcode.InvalidParams
	}
	if p.NTCBiasOhm == 0 {
		p.NTCBiasOhm = 10_000
	}
	if p.R25Ohm == 0 {
		p.R25Ohm = 10_000
	}
	if p.BetaK == 0 {
		p.BetaK = 3435
	}

	// Claim I2C (serialised by provider) and SMBALERT pin for input.
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
	// Ensure pull-up; LTC4015 SMBALERT# is open-drain, active-low.
	_ = gpio.ConfigureInput(core.PullUp)

	name := p.Name
	domBat := p.DomainBattery
	domChg := p.DomainCharger

	dev := &Device{
		id:   in.ID,
		aBat: core.CapAddr{Domain: domBat, Kind: types.KindBattery, Name: name},
		aChg: core.CapAddr{Domain: domChg, Kind: types.KindCharger, Name: name},
		aTmp: core.CapAddr{Domain: domChg, Kind: types.KindTemperature, Name: name},

		res:  in.Res,
		i2c:  i2c,
		pin:  p.SMBAlertPin,
		gpio: gpio,

		params: p,
	}

	return dev, nil
}

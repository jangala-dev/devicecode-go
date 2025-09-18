// services/hal/internal/devices/ltc4015/driver_rp2xxx.go
//go:build rp2040 || rp2350

package ltc4015adpt

import (
	drv "devicecode-go/drivers/ltc4015"

	"tinygo.org/x/drivers"
)

type rp2Dev struct{ d *drv.Device }

func newLTC4015(i2c drivers.I2C, cfg configLite) (ltcDev, error) {
	dcfg := drv.Config{
		Address:         cfg.Address,
		RSNSB_uOhm:      cfg.RSNSB_uOhm,
		RSNSI_uOhm:      cfg.RSNSI_uOhm,
		Cells:           cfg.Cells,
		QCountPrescale:  cfg.QCountPrescale,
		TargetsWritable: cfg.TargetsWritable,
	}
	switch cfg.Chem {
	case chemLithium:
		dcfg.Chem = drv.ChemLithium
	case chemLeadAcid:
		dcfg.Chem = drv.ChemLeadAcid
	default:
		dcfg.Chem = drv.ChemUnknown
	}

	var dev *drv.Device
	var err error
	if dcfg.Chem == drv.ChemUnknown {
		dev, err = drv.NewAuto(i2c, dcfg)
	} else {
		dev = drv.New(i2c, dcfg)
	}
	if err != nil {
		return nil, err
	}

	// Apply convenience flags.
	if cfg.ForceMeasSysOn {
		_ = dev.SetConfigBits(drv.ForceMeasSysOn)
	}
	if cfg.EnableQCount {
		_ = dev.SetConfigBits(drv.EnableQCount)
	}

	return &rp2Dev{d: dev}, nil
}

func (w *rp2Dev) Configure(c configLite) error {
	// Best-effort on changed fields only.
	return w.d.Configure(drv.Config{
		Address:         c.Address,
		RSNSB_uOhm:      c.RSNSB_uOhm,
		RSNSI_uOhm:      c.RSNSI_uOhm,
		Cells:           c.Cells,
		QCountPrescale:  c.QCountPrescale,
		TargetsWritable: c.TargetsWritable,
		Chem: func() drv.Chemistry {
			switch c.Chem {
			case chemLithium:
				return drv.ChemLithium
			case chemLeadAcid:
				return drv.ChemLeadAcid
			default:
				return drv.ChemUnknown
			}
		}(),
	})
}

func (w *rp2Dev) Chemistry() string {
	switch w.d.Chem() {
	case drv.ChemLithium:
		return "lithium"
	case drv.ChemLeadAcid:
		return "lead_acid"
	default:
		return "unknown"
	}
}

func (w *rp2Dev) Cells() uint8                         { return w.d.Cells() }
func (w *rp2Dev) MeasSystemValid() (bool, error)       { return w.d.MeasSystemValid() }
func (w *rp2Dev) BatteryMilliVPerCell() (int32, error) { return w.d.BatteryMilliVPerCell() }
func (w *rp2Dev) BatteryMilliVPack() (int32, error)    { return w.d.BatteryMilliVPack() }
func (w *rp2Dev) VinMilliV() (int32, error)            { return w.d.VinMilliV() }
func (w *rp2Dev) VsysMilliV() (int32, error)           { return w.d.VsysMilliV() }
func (w *rp2Dev) IbatMilliA() (int32, error)           { return w.d.IbatMilliA() }
func (w *rp2Dev) IinMilliA() (int32, error)            { return w.d.IinMilliA() }
func (w *rp2Dev) DieMilliC() (int32, error)            { return w.d.DieMilliC() }
func (w *rp2Dev) BSRMicroOhmPerCell() (uint32, error)  { return w.d.BSRMicroOhmPerCell() }
func (w *rp2Dev) IChargeDAC_mA() (int32, error)        { return w.d.IChargeDAC_mA() }
func (w *rp2Dev) IinLimitDAC_mA() (int32, error)       { return w.d.IinLimitDAC_mA() }
func (w *rp2Dev) IChargeBSR_mA() (int32, error)        { return w.d.IChargeBSR_mA() }
func (w *rp2Dev) SetIinLimit_mA(v int32) error         { return w.d.SetIinLimit_mA(v) }
func (w *rp2Dev) SetIChargeTarget_mA(v int32) error    { return w.d.SetIChargeTarget_mA(v) }
func (w *rp2Dev) SetVinUvcl_mV(v int32) error          { return w.d.SetVinUvcl_mV(v) }
func (w *rp2Dev) AlertActive(get func() bool) bool     { return w.d.AlertActive(get) }
func (w *rp2Dev) SetSuspend(on bool) error {
	if on {
		return w.d.SetConfigBits(drv.SuspendCharger)
	}
	return w.d.ClearConfigBits(drv.SuspendCharger)
}

func (w *rp2Dev) RawStatus() (uint16, uint16, uint16, error) {
	ss, err := w.d.SystemStatus()
	if err != nil {
		return 0, 0, 0, err
	}
	cs, err := w.d.ChargerState()
	if err != nil {
		return 0, 0, 0, err
	}
	st, err := w.d.ChargeStatus()
	if err != nil {
		return 0, 0, 0, err
	}
	return uint16(ss), uint16(cs), uint16(st), nil
}

func (w *rp2Dev) Summary() (StatusSummary, error) {
	ss, _ := w.d.SystemStatus()
	cs, _ := w.d.ChargerState()
	st, _ := w.d.ChargeStatus()
	return StatusSummary{
		Equalize:        cs.Has(drv.EqualizeCharge),
		Absorb:          cs.Has(drv.AbsorbCharge),
		Precharge:       cs.Has(drv.Precharge),
		Suspended:       cs.Has(drv.ChargerSuspended),
		InCCCV:          cs.Has(drv.CCCVCharge),
		CC:              st.Has(drv.ConstCurrent),
		CV:              st.Has(drv.ConstVoltage),
		OkToCharge:      ss.Has(drv.OkToCharge),
		BatMissing:      cs.Has(drv.BatMissingFault),
		BatShort:        cs.Has(drv.BatShortFault),
		ThermalShutdown: ss.Has(drv.ThermalShutdown),
		VinUvcl:         st.Has(drv.VinUvclActive),
		IinLimit:        st.Has(drv.IinLimitActive),
	}, nil
}

func (w *rp2Dev) DrainAlerts() (AlertEventRaw, error) {
	ev, err := w.d.DrainAlerts()
	return AlertEventRaw{Limit: uint16(ev.Limit), ChgState: uint16(ev.ChgState), ChgStatus: uint16(ev.ChgStatus)}, err
}

func (w *rp2Dev) ServiceSMBAlert() (AlertEventRaw, bool, error) {
	ev, ok, err := w.d.ServiceSMBAlert()
	return AlertEventRaw{Limit: uint16(ev.Limit), ChgState: uint16(ev.ChgState), ChgStatus: uint16(ev.ChgStatus)}, ok, err
}

func (w *rp2Dev) LeadAcid() (laView, bool) {
	la, ok := w.d.LeadAcid()
	if !ok {
		return nil, false
	}
	return la, true // drv.LeadAcid implements the laView methods
}

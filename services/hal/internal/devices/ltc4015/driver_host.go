// services/hal/internal/devices/ltc4015/driver_host.go
//go:build !rp2040 && !rp2350

package ltc4015

import (
	"sync/atomic"
	"time"

	"tinygo.org/x/drivers"
)

// Simple host simulator implementing ltcDev without any TinyGo chip driver deps.

type simDev struct {
	cfg                 configLite
	chem                chemistry
	cells               uint8
	qcount              uint32
	alertTick           uint32
	vin_mV              int32
	vsys_mV             int32
	vbat_pc             int32
	iin_mA              int32
	ibat_mA             int32
	die_mC              int32
	bsr_uohm            uint32
	iin_limit           int32
	ichg_dac            int32
	sum                 StatusSummary
	rawSS, rawCS, rawST uint16
	suspended           bool
}

func newLTC4015(_ drivers.I2C, cfg configLite) (ltcDev, error) {
	return &simDev{
		cfg:      cfg,
		chem:     cfg.Chem,
		cells:    cfg.Cells,
		vin_mV:   12000,
		vsys_mV:  5000,
		vbat_pc:  3600,
		iin_mA:   1000,
		ibat_mA:  800,
		die_mC:   42000,
		bsr_uohm: 1500,
		sum:      StatusSummary{InCCCV: true, CC: true, OkToCharge: true},
		rawSS:    0x0001, rawCS: 0x0040, rawST: 0x0002, // placeholders
	}, nil
}

func (s *simDev) Configure(c configLite) error {
	if c.Cells != 0 {
		s.cells = c.Cells
	}
	return nil
}
func (s *simDev) Chemistry() string {
	switch s.chem {
	case chemLithium:
		return "lithium"
	case chemLeadAcid:
		return "lead_acid"
	default:
		return "unknown"
	}
}
func (s *simDev) Cells() uint8 { return s.cells }

func (s *simDev) MeasSystemValid() (bool, error) { return true, nil }

func (s *simDev) BatteryMilliVPerCell() (int32, error) { return s.vbat_pc, nil }
func (s *simDev) BatteryMilliVPack() (int32, error) {
	if s.cells == 0 {
		return s.vbat_pc, nil
	}
	return s.vbat_pc * int32(s.cells), nil
}
func (s *simDev) VinMilliV() (int32, error)           { return s.vin_mV, nil }
func (s *simDev) VsysMilliV() (int32, error)          { return s.vsys_mV, nil }
func (s *simDev) IbatMilliA() (int32, error)          { return s.ibat_mA, nil }
func (s *simDev) IinMilliA() (int32, error)           { return s.iin_mA, nil }
func (s *simDev) DieMilliC() (int32, error)           { return s.die_mC, nil }
func (s *simDev) BSRMicroOhmPerCell() (uint32, error) { return s.bsr_uohm, nil }
func (s *simDev) IChargeDAC_mA() (int32, error)       { return s.ichg_dac, nil }
func (s *simDev) IinLimitDAC_mA() (int32, error)      { return s.iin_limit, nil }
func (s *simDev) IChargeBSR_mA() (int32, error)       { return s.ibat_mA, nil }

func (s *simDev) Summary() (StatusSummary, error)            { return s.sum, nil }
func (s *simDev) RawStatus() (uint16, uint16, uint16, error) { return s.rawSS, s.rawCS, s.rawST, nil }

func (s *simDev) SetIinLimit_mA(v int32) error { s.iin_limit = v; s.sum.IinLimit = true; return nil }
func (s *simDev) SetIChargeTarget_mA(v int32) error {
	s.ichg_dac = v
	s.sum.CC, s.sum.CV = true, false
	return nil
}
func (s *simDev) SetVinUvcl_mV(int32) error { s.sum.VinUvcl = true; return nil }

func (s *simDev) DrainAlerts() (AlertEventRaw, error) {
	if atomic.AddUint32(&s.alertTick, 1)%8 == 0 {
		return AlertEventRaw{Limit: 1 << 8 /* VINHi placeholder */}, nil
	}
	return AlertEventRaw{}, nil
}
func (s *simDev) ServiceSMBAlert() (AlertEventRaw, bool, error) {
	ev, _ := s.DrainAlerts()
	if ev.Limit|ev.ChgState|ev.ChgStatus != 0 {
		return ev, true, nil
	}
	return AlertEventRaw{}, false, nil
}
func (s *simDev) AlertActive(get func() bool) bool {
	if get == nil {
		return time.Now().UnixNano()/1e6%5000 < 10
	}
	return !get()
}

func (s *simDev) SetSuspend(on bool) error { s.suspended = on; s.sum.Suspended = on; return nil }

func (s *simDev) LeadAcid() (laView, bool) { return laSim{s}, s.chem == chemLeadAcid }

// laSim implements laView on the simulator.
type laSim struct{ s *simDev }

func (la laSim) SetVChargeSetting_mVPerCell(mV int32, _ bool) error { la.s.vbat_pc = mV; return nil }
func (la laSim) SetVAbsorbDelta_mVPerCell(int32) error              { return nil }
func (la laSim) SetVEqualizeDelta_mVPerCell(int32) error            { return nil }
func (la laSim) SetMaxAbsorbTime_s(uint16) error                    { return nil }
func (la laSim) SetEqualizeTime_s(uint16) error                     { return nil }
func (la laSim) EnableLeadAcidTempComp(bool) error                  { return nil }

// Package ltc4015 provides a TinyGo driver for the LTC4015 multi-chemistry
// synchronous buck battery charger. The public API is integer-only. Methods
// are not safe for concurrent use and should be serialised by the caller.
package ltc4015

import (
	"errors"

	"tinygo.org/x/drivers"
)

// ---------------- Types and configuration ----------------

type Chemistry uint8

const (
	ChemUnknown  Chemistry = iota
	ChemLithium            // VBAT LSB: 192.264 µV/cell
	ChemLeadAcid           // VBAT LSB: 128.176 µV/cell
)

var (
	ErrTargetsReadOnly  = errors.New("targets/timers are read-only in fixed-chem mode")
	ErrChemistryUnknown = errors.New("unable to determine chemistry")
)

type Config struct {
	Address         uint16
	RSNSB_uOhm      uint32
	RSNSI_uOhm      uint32
	Cells           uint8
	Chem            Chemistry
	QCountPrescale  uint16 // if 0, leave hardware default
	TargetsWritable bool   // set false if using a fixed-chem variant (guards 0x1A–0x2D)
}

// DefaultConfig returns a minimal configuration. Sense resistors must be set by
// the caller to use current-related APIs.
func DefaultConfig() Config {
	return Config{
		Address:         AddressDefault,
		Chem:            ChemLithium, // default unless overridden/detected
		TargetsWritable: true,
	}
}

// Validate performs basic checks on values that are required by many APIs.
func (c Config) Validate() error {
	if c.Address == 0 {
		return errors.New("Address must be non-zero (use AddressDefault)")
	}
	if c.RSNSB_uOhm == 0 {
		return errors.New("RSNSB_uOhm must be set (battery path sense)")
	}
	if c.RSNSI_uOhm == 0 {
		return errors.New("RSNSI_uOhm must be set (input path sense)")
	}
	return nil
}

type Device struct {
	i2c   drivers.I2C
	addr  uint16
	cells uint8
	chem  Chemistry

	rsnsB_uOhm      uint32
	rsnsI_uOhm      uint32
	targetsWritable bool

	// Fixed buffers to avoid per-call heap allocations.
	w [3]byte
	r [2]byte
}

func New(i2c drivers.I2C, cfg Config) *Device {
	addr := cfg.Address
	if addr == 0 {
		addr = AddressDefault
	}
	chem := cfg.Chem
	if chem == ChemUnknown {
		chem = ChemLithium
	}
	return &Device{
		i2c:             i2c,
		addr:            addr,
		cells:           cfg.Cells,
		chem:            chem,
		rsnsB_uOhm:      cfg.RSNSB_uOhm,
		rsnsI_uOhm:      cfg.RSNSI_uOhm,
		targetsWritable: cfg.TargetsWritable,
	}
}

func (d *Device) Configure(cfg Config) error {
	// Chemistry is treated as fixed after construction; do not change here.
	if cfg.Cells != 0 {
		d.cells = cfg.Cells
	} else {
		if v, err := d.readWord(regChemCells); err == nil {
			d.cells = uint8(v & 0x000F) // pins-based cell count (bits 3:0)
		}
	}
	if cfg.RSNSB_uOhm != 0 {
		d.rsnsB_uOhm = cfg.RSNSB_uOhm
	}
	if cfg.RSNSI_uOhm != 0 {
		d.rsnsI_uOhm = cfg.RSNSI_uOhm
	}
	if cfg.QCountPrescale != 0 {
		if err := d.writeWord(regQCountPrescale, cfg.QCountPrescale); err != nil {
			return err
		}
	}
	// TargetsWritable may be updated if the caller knows the variant.
	if !cfg.TargetsWritable {
		d.targetsWritable = false
	}
	return nil
}

// Introspection.
func (d *Device) Chem() Chemistry { return d.chem }
func (d *Device) Cells() uint8    { return d.cells }

// ---------------- Helpers ----------------

func (d *Device) ensureTargetsWritable() error {
	if !d.targetsWritable {
		return ErrTargetsReadOnly
	}
	return nil
}

// ---------------- Generic bitmask register control ----------------

func (d *Device) modifyBitmaskRegister(regAddr byte, set, clear uint16) error {
	current, err := d.readWord(regAddr)
	if err != nil {
		return err
	}
	newVal := (current | set) &^ clear
	return d.writeWord(regAddr, newVal)
}

// ---------------- CONFIG_BITS control (typed, minimal API) ----------------

func (b ConfigBits) Has(flag ConfigBits) bool { return b&flag != 0 }

func (d *Device) ReadConfig() (ConfigBits, error) {
	v, err := d.readWord(regConfigBits)
	return ConfigBits(v), err
}

func (d *Device) WriteConfig(v ConfigBits) error {
	return d.writeWord(regConfigBits, uint16(v))
}

func (d *Device) SetConfigBits(mask ConfigBits) error {
	return d.modifyBitmaskRegister(regConfigBits, uint16(mask), 0)
}

func (d *Device) ClearConfigBits(mask ConfigBits) error {
	return d.modifyBitmaskRegister(regConfigBits, 0, uint16(mask))
}

func (d *Device) UpdateConfig(set, clear ConfigBits) error {
	return d.modifyBitmaskRegister(regConfigBits, uint16(set), uint16(clear))
}

// ---------------- CHARGER_CONFIG_BITS control ----------------

func (b ChargerCfgBits) Has(flag ChargerCfgBits) bool { return b&flag != 0 }

func (d *Device) ReadChargerConfig() (ChargerCfgBits, error) {
	v, err := d.readWord(regChargerCfgBits)
	return ChargerCfgBits(v), err
}

func (d *Device) WriteChargerConfig(v ChargerCfgBits) error {
	return d.writeWord(regChargerCfgBits, uint16(v))
}

func (d *Device) SetChargerConfigBits(mask ChargerCfgBits) error {
	return d.modifyBitmaskRegister(regChargerCfgBits, uint16(mask), 0)
}

func (d *Device) ClearChargerConfigBits(mask ChargerCfgBits) error {
	return d.modifyBitmaskRegister(regChargerCfgBits, 0, uint16(mask))
}

func (d *Device) UpdateChargerConfig(set, clear ChargerCfgBits) error {
	return d.modifyBitmaskRegister(regChargerCfgBits, uint16(set), uint16(clear))
}

// ---------------- Bitmask helpers ----------------

func (b LimitBits) Has(flag LimitBits) bool               { return b&flag != 0 }
func (b ChargerStateBits) Has(flag ChargerStateBits) bool { return b&flag != 0 }
func (b ChargeStatusBits) Has(flag ChargeStatusBits) bool { return b&flag != 0 }
func (b SystemStatus) Has(flag SystemStatus) bool         { return b&flag != 0 }

// ---------------- Chemistry/Cell detection ----------------

func (d *Device) DetectCells() (uint8, error) {
	v, err := d.readWord(regChemCells)
	if err != nil {
		return 0, err
	}
	return uint8(v & 0x000F), nil
}

func (d *Device) DetectChemistry() (Chemistry, error) {
	v, err := d.readWord(regChemCells)
	if err != nil {
		return ChemUnknown, err
	}
	chemCode := (v >> 8) & 0x000F
	switch chemCode {
	case 0x7, 0x8:
		return ChemLeadAcid, nil
	case 0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6:
		return ChemLithium, nil // includes Li-Ion and LiFePO4 families
	default:
		return ChemUnknown, nil
	}
}

// ---------------- Telemetry (integer units) ----------------

func (d *Device) BatteryMilliVPerCell() (int32, error) {
	raw, err := d.readWord(regVBAT)
	if err != nil {
		return 0, err
	}
	// Li: 192,264 nV/LSB; Lead: 128,176 nV/LSB.
	nV := int64(192264)
	if d.chem == ChemLeadAcid {
		nV = 128176
	}
	uV := (int64(raw) * nV) / 1000 // nV→µV
	return int32(uV / 1000), nil   // µV→mV
}

func (d *Device) BatteryMilliVPack() (int32, error) {
	perCell, err := d.BatteryMilliVPerCell()
	if err != nil {
		return 0, err
	}
	if d.cells == 0 {
		return perCell, nil
	}
	return perCell * int32(d.cells), nil
}

func (d *Device) VinMilliV() (int32, error) {
	raw, err := d.readWord(regVIN)
	if err != nil {
		return 0, err
	}
	uV := int64(raw) * 1648
	return int32(uV / 1000), nil
}

func (d *Device) VsysMilliV() (int32, error) {
	raw, err := d.readWord(regVSYS)
	if err != nil {
		return 0, err
	}
	uV := int64(raw) * 1648
	return int32(uV / 1000), nil
}

func (d *Device) IbatMilliA() (int32, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, errors.New("RSNSB_uOhm not set")
	}
	raw, err := d.readS16(regIBAT)
	if err != nil {
		return 0, err
	}
	uA := (int64(raw) * 1464870) / int64(d.rsnsB_uOhm)
	return int32(uA / 1000), nil
}

func (d *Device) IinMilliA() (int32, error) {
	if d.rsnsI_uOhm == 0 {
		return 0, errors.New("RSNSI_uOhm not set")
	}
	raw, err := d.readS16(regIIN)
	if err != nil {
		return 0, err
	}
	uA := (int64(raw) * 1464870) / int64(d.rsnsI_uOhm)
	return int32(uA / 1000), nil
}

func (d *Device) DieMilliC() (int32, error) {
	raw, err := d.readS16(regDieTemp)
	if err != nil {
		return 0, err
	}
	return int32((int64(raw) - 12010) * 10000 / 456), nil
}

func (d *Device) BSRMicroOhmPerCell() (uint32, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, errors.New("RSNSB_uOhm not set")
	}
	raw, err := d.readWord(regBSR)
	if err != nil {
		return 0, err
	}
	div := int64(500)
	if d.chem == ChemLeadAcid {
		div = 750
	}
	return uint32((int64(raw) * int64(d.rsnsB_uOhm)) / div), nil
}

func (d *Device) MeasSystemValid() (bool, error) {
	v, err := d.readWord(regMeasSysValid)
	if err != nil {
		return false, err
	}
	return (v & 0x0001) != 0, nil
}

// ---------------- Effective DAC read-backs ----------------

// Raw codes
func (d *Device) IChargeDACCode() (uint16, error)  { return d.readWord(regIChargeDAC) }
func (d *Device) VChargeDACCode() (uint16, error)  { return d.readWord(regVChargeDAC) }
func (d *Device) IinLimitDACCode() (uint16, error) { return d.readWord(regIinLimitDAC) }

// Convenience in physical units (requires RSNS values).
func (d *Device) IChargeDAC_mA() (int32, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, errors.New("RSNSB_uOhm not set")
	}
	code, err := d.IChargeDACCode()
	if err != nil {
		return 0, err
	}
	// I = ((code+1)*1 mV)/RSNSB
	mA := (int64(code) + 1) * 1_000_000 / int64(d.rsnsB_uOhm)
	return int32(mA), nil
}

func (d *Device) IinLimitDAC_mA() (int32, error) {
	if d.rsnsI_uOhm == 0 {
		return 0, errors.New("RSNSI_uOhm not set")
	}
	code, err := d.IinLimitDACCode()
	if err != nil {
		return 0, err
	}
	// I = ((code+1)*0.5 mV)/RSNSI
	mA := (int64(code) + 1) * 500_000 / int64(d.rsnsI_uOhm)
	return int32(mA), nil
}

// ---------------- Typed status readers ----------------

func (d *Device) SystemStatus() (SystemStatus, error) {
	v, err := d.readWord(regSystemStatus)
	return SystemStatus(v), err
}

func (d *Device) ChargerState() (ChargerState, error) {
	v, err := d.readWord(regChargerState)
	return ChargerState(v), err
}

func (d *Device) ChargeStatus() (ChargeStatus, error) {
	v, err := d.readWord(regChargeStatus)
	return ChargeStatus(v), err
}

// ---------------- Limits, enables, alerts ----------------

func (d *Device) SetVBATWindow_mVPerCell(lo_mV, hi_mV int32) error {
	if err := d.writeWord(regVBATLoAlertLimit, d.toVBATCode(lo_mV)); err != nil {
		return err
	}
	return d.writeWord(regVBATHiAlertLimit, d.toVBATCode(hi_mV))
}

func (d *Device) SetVINWindow_mV(lo_mV, hi_mV int32) error {
	if err := d.writeWord(regVINLoAlertLimit, d.toCode_1p648mV_LSB(lo_mV)); err != nil {
		return err
	}
	return d.writeWord(regVINHiAlertLimit, d.toCode_1p648mV_LSB(hi_mV))
}

func (d *Device) SetVSYSWindow_mV(lo_mV, hi_mV int32) error {
	if err := d.writeWord(regVSYSLoAlertLimit, d.toCode_1p648mV_LSB(lo_mV)); err != nil {
		return err
	}
	return d.writeWord(regVSYSHiAlertLimit, d.toCode_1p648mV_LSB(hi_mV))
}

func (d *Device) SetIINHigh_mA(mA int32) error {
	if d.rsnsI_uOhm == 0 {
		return errors.New("RSNSI_uOhm not set")
	}
	return d.writeWord(regIINHiAlertLimit, d.currCode(mA, d.rsnsI_uOhm))
}

func (d *Device) SetIBATLow_mA(mA int32) error {
	if d.rsnsB_uOhm == 0 {
		return errors.New("RSNSB_uOhm not set")
	}
	return d.writeWord(regIBATLoAlertLimit, d.currCode(mA, d.rsnsB_uOhm))
}

func (d *Device) SetDieTempHigh_mC(mC int32) error {
	raw := int64(12010) + (int64(456)*int64(mC))/10000
	return d.writeWord(regDieTempHiAlertLimit, clamp16(raw))
}

func (d *Device) SetBSRHigh_uOhmPerCell(uOhm uint32) error {
	if d.rsnsB_uOhm == 0 {
		return errors.New("RSNSB_uOhm not set")
	}
	div := int64(500)
	if d.chem == ChemLeadAcid {
		div = 750
	}
	raw := (int64(uOhm) * div) / int64(d.rsnsB_uOhm)
	return d.writeWord(regBSRHiAlertLimit, clamp16(raw))
}

func (d *Device) SetNTCRatioWindowRaw(hi, lo uint16) error {
	if err := d.writeWord(regNTCRatioHiAlertLimit, hi); err != nil {
		return err
	}
	return d.writeWord(regNTCRatioLoAlertLimit, lo)
}

func (d *Device) EnableLimitAlertsMask(mask LimitEnable) error {
	return d.writeWord(regEnLimitAlerts, uint16(mask))
}

func (d *Device) EnableChargerStateAlertsMask(mask ChargerStateEnable) error {
	return d.writeWord(regEnChargerStAlerts, uint16(mask))
}

func (d *Device) EnableChargeStatusAlertsMask(mask ChargeStatusEnable) error {
	return d.writeWord(regEnChargeStAlerts, uint16(mask))
}

func (d *Device) ReadLimitAlerts() (LimitAlerts, error) {
	v, err := d.readWord(regLimitAlerts)
	return LimitAlerts(v), err
}

func (d *Device) ReadChargerStateAlerts() (ChargerStateAlerts, error) {
	v, err := d.readWord(regChargerStateAlert)
	return ChargerStateAlerts(v), err
}

func (d *Device) ReadChargeStatusAlerts() (ChargeStatusAlerts, error) {
	v, err := d.readWord(regChargeStatAlerts)
	return ChargeStatusAlerts(v), err
}

func (d *Device) ClearLimitAlerts() error        { return d.writeWord(regLimitAlerts, 0x0000) }
func (d *Device) ClearChargerStateAlerts() error { return d.writeWord(regChargerStateAlert, 0x0000) }
func (d *Device) ClearChargeStatusAlerts() error { return d.writeWord(regChargeStatAlerts, 0x0000) }

// ---------------- Coulomb counter (minimal) ----------------

func (d *Device) SetQCount(v uint16) error { return d.writeWord(regQCount, v) }
func (d *Device) QCount() (uint16, error)  { return d.readWord(regQCount) }
func (d *Device) SetQCountLimits(lo, hi uint16) error {
	if err := d.writeWord(regQCountLoLimit, lo); err != nil {
		return err
	}
	return d.writeWord(regQCountHiLimit, hi)
}
func (d *Device) SetQCountPrescale(p uint16) error { return d.writeWord(regQCountPrescale, p) }

// ---------------- Input parameter setting ----------------

// Input current limit: (code+1)*500 µV across RSNSI, 0..63.
func (d *Device) SetIinLimit_mA(mA int32) error {
	if d.rsnsI_uOhm == 0 {
		return errors.New("RSNSI_uOhm not set")
	}
	v_uV := (int64(mA) * int64(d.rsnsI_uOhm)) / 1000 // µV across RSNSI
	code := qLinear(v_uV, 500 /*µV*/, 0, true, 0, 63)
	return d.writeWord(regIinLimitSetting, code)
}

// VIN_UVCL: (code+1)*4.6875 mV, 0..255 (nanovolts avoid fractional LSB).
func (d *Device) SetVinUvcl_mV(mV int32) error {
	v_nV := int64(mV) * 1_000_000
	step_nV := int64(4_687_500)
	code := qLinear(v_nV, step_nV, 0, true, 0, 255)
	return d.writeWord(regVinUvclSetting, code)
}

// ---------------- Charge parameter setting ----------------

// ICHARGE_TARGET: (code+1)*1 mV across RSNSB, 0..31.
func (d *Device) SetIChargeTarget_mA(mA int32) error {
	if err := d.ensureTargetsWritable(); err != nil {
		return err
	}
	if d.rsnsB_uOhm == 0 {
		return errors.New("RSNSB_uOhm not set")
	}
	v_uV := (int64(mA) * int64(d.rsnsB_uOhm)) / 1000 // µV across RSNSB
	code := qLinear(v_uV, 1000 /*µV = 1 mV*/, 0, true, 0, 31)
	return d.writeWord(regIChargeTarget, code)
}

// ---------------- Low-level SMBus (READ/WRITE WORD & ARA) ----------------

func (d *Device) AcknowledgeAlert() (bool, error) {
	var r [1]byte
	if err := d.i2c.Tx(ARAAddress, nil, r[:]); err != nil {
		return false, err
	}
	expected := byte((d.addr << 1) | 1)
	return r[0] == expected, nil
}

type PinInput func() bool // returns logical level

// SMBALERT is active-low; this helper keeps the driver portable.
func (d *Device) AlertActive(get PinInput) bool { return !get() }

// --- bus word primitives

func (d *Device) readWord(reg byte) (uint16, error) {
	d.w[0] = reg
	if err := d.i2c.Tx(d.addr, d.w[:1], d.r[:2]); err != nil {
		return 0, err
	}
	// Little-endian: LOW then HIGH.
	return uint16(d.r[0]) | uint16(d.r[1])<<8, nil
}

func (d *Device) readS16(reg byte) (int16, error) {
	u, err := d.readWord(reg)
	return int16(u), err
}

func (d *Device) writeWord(reg byte, val uint16) error {
	d.w[0] = reg
	d.w[1] = byte(val)      // low
	d.w[2] = byte(val >> 8) // high
	return d.i2c.Tx(d.addr, d.w[:3], nil)
}

// ---------------- Integer scaling helpers ----------------

func clampRange(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp16(v int64) uint16 {
	return uint16(int16(clampRange(v, -32768, 32767)))
}

// qLinear maps a physical value onto a linear code:
//
//	code_physical = (code + addOne?1:0)*step + offset
//
// inverse:
//
//	code = round((value - offset)/step) - (addOne?1:0)
//
// All parameters are integers in consistent units (e.g. nV or µV).
func qLinear(value, step, offset int64, addOne bool, lo, hi int64) uint16 {
	num := value - offset
	if num < 0 {
		num = 0
	}
	code := (num + step/2) / step
	if addOne && code > 0 {
		code--
	}
	code = clampRange(code, lo, hi)
	return uint16(code)
}

// laCountsFrommV provides the lead-acid voltage-count mapping used by
// VCHARGE_SETTING and the absorb/equalise deltas:
//
//	Vcell ≈ 2000 mV + code*(1000/105 mV)
//
// Therefore: code ≈ round(105*(mV - offset_mV)/1000).
func laCountsFrommV(mV, offset_mV int32) int64 {
	return (int64(mV-offset_mV)*105 + 500) / 1000
}

func (d *Device) toVBATCode(mV int32) uint16 {
	nV := int64(192264)
	if d.chem == ChemLeadAcid {
		nV = 128176
	}
	code := (int64(mV)*1_000_000 + nV/2) / nV
	return clamp16(code)
}

func (d *Device) toCode_1p648mV_LSB(mV int32) uint16 {
	const nV = 1_648_000
	code := (int64(mV)*1_000_000 + nV/2) / nV
	return clamp16(code)
}

func (d *Device) currCode(mA int32, rsns_uOhm uint32) uint16 {
	const pVperLSB = 1_464_870
	uA := int64(mA) * 1000
	code := (uA*int64(rsns_uOhm) + pVperLSB/2) / pVperLSB
	return clamp16(code)
}

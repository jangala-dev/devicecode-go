// Package ltc4015 provides a minimal TinyGo driver for the LTC4015
// multi-chemistry synchronous buck battery charger.
//
// Design notes (datasheet references):
// • I2C/SMBus, 400kHz, read/write word protocol; data-low then data-high.
// • Default 7-bit address = 0b1101000.
// • Integer-only telemetry scaling (VBAT, VIN, VSYS, IBAT, IIN, DIE_TEMP, NTC_RATIO, BSR).
// • System status and alert registers with clear-on-write-0 behaviour.
// • Limit alert enables / limits and Coulomb counter controls.

package ltc4015

import (
	"errors"

	"tinygo.org/x/drivers"
)

// ---------------- Top level vars ----------------

var ErrNotApplicable = errors.New("not applicable for current chemistry")

// ---------------- Types and configuration ----------------

type Chemistry uint8

const (
	ChemUnknown  Chemistry = iota
	ChemLithium            // VBAT LSB: 192.264 µV/cell
	ChemLeadAcid           // VBAT LSB: 128.176 µV/cell
)

type Config struct {
	Address        uint16
	RSNSB_uOhm     uint32
	RSNSI_uOhm     uint32
	Cells          uint8
	Chem           Chemistry
	QCountPrescale uint16 // if 0, leave hardware default
}

type Device struct {
	i2c   drivers.I2C
	addr  uint16
	cells uint8
	chem  Chemistry

	rsnsB_uOhm uint32
	rsnsI_uOhm uint32

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
		i2c:        i2c,
		addr:       addr,
		cells:      cfg.Cells,
		chem:       chem,
		rsnsB_uOhm: cfg.RSNSB_uOhm,
		rsnsI_uOhm: cfg.RSNSI_uOhm,
	}
}

func (d *Device) Configure(cfg Config) error {
	if cfg.Cells != 0 {
		d.cells = cfg.Cells
	} else {
		if v, err := d.readWord(regChemCells); err == nil {
			d.cells = uint8(v & 0x000F) // pins-based cell count (bits 3:0)
		}
	}
	if cfg.Chem != ChemUnknown {
		d.chem = cfg.Chem
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
	return nil
}

// ---------------- Generic bitmask register control ----------------

// modifyBitmaskRegister is a private helper for the read-modify-write pattern
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

// ---------------- CHARGER_CONFIG_BITS control (typed, minimal API) ----------------

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

// DetectCells returns the pin-defined cell count.
func (d *Device) DetectCells() (uint8, error) {
	v, err := d.readWord(regChemCells)
	if err != nil {
		return 0, err
	}
	return uint8(v & 0x000F), nil
}

// DetectChemistry returns the device-reported chemistry.
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

// Typed status readers.

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

// Threshold setters.

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

// Enable masks (absolute write; compact API).

func (d *Device) EnableLimitAlertsMask(mask LimitEnable) error {
	return d.writeWord(regEnLimitAlerts, uint16(mask))
}

func (d *Device) EnableChargerStateAlertsMask(mask ChargerStateEnable) error {
	return d.writeWord(regEnChargerStAlerts, uint16(mask))
}

func (d *Device) EnableChargeStatusAlertsMask(mask ChargeStatusEnable) error {
	return d.writeWord(regEnChargeStAlerts, uint16(mask))
}

// Alert reads/clears.

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
	// µV across RSNSI = (mA * µΩ)/1000
	v_uV := (int64(mA) * int64(d.rsnsI_uOhm)) / 1000
	code := qLinear(v_uV, 500 /*µV*/, 0, true, 0, 63)
	return d.writeWord(regIinLimitSetting, code)
}

// VIN_UVCL: (code+1)*4.6875 mV, 0..255.
// Use nanovolts to avoid fractional LSB.
func (d *Device) SetVinUvcl_mV(mV int32) error {
	v_nV := int64(mV) * 1_000_000 // mV → nV
	step_nV := int64(4_687_500)   // 4.6875 mV in nV
	code := qLinear(v_nV, step_nV, 0, true, 0, 255)
	return d.writeWord(regVinUvclSetting, code)
}

// ---------------- Charge parameter setting ----------------

// ICHARGE_TARGET: (code+1)*1 mV across RSNSB, 0..31.
func (d *Device) SetIChargeTarget_mA(mA int32) error {
	if d.rsnsB_uOhm == 0 {
		return errors.New("RSNSB_uOhm not set")
	}
	v_uV := (int64(mA) * int64(d.rsnsB_uOhm)) / 1000 // µV across RSNSB
	code := qLinear(v_uV, 1000 /*µV = 1 mV*/, 0, true, 0, 31)
	return d.writeWord(regIChargeTarget, code)
}

// Lead-acid only: VCHARGE_SETTING per-cell.
// code ≈ round(105*(mV - 2000)/1000); clamp 0..63; optional cap 35 with tempComp.
func (d *Device) SetVChargeSetting_mVPerCell(mV int32, tempComp bool) error {
	if d.chem != ChemLeadAcid {
		return ErrNotApplicable
	}
	code := laCountsFrommV(mV, 2000)
	if tempComp && code > 35 {
		code = 35
	}
	code = clampRange(code, 0, 63)
	return d.writeWord(regVChargeSetting, uint16(code))
}

// Lead-acid only: absorb delta per-cell, same scaling (offset 0).
func (d *Device) SetVAbsorbDelta_mVPerCell(delta int32) error {
	if d.chem != ChemLeadAcid {
		return ErrNotApplicable
	}
	code := clampRange(laCountsFrommV(delta, 0), 0, 63)
	return d.writeWord(regVAbsorbDelta, uint16(code))
}

// Lead-acid only: equalise delta per-cell, same scaling (offset 0).
func (d *Device) SetVEqualizeDelta_mVPerCell(delta int32) error {
	if d.chem != ChemLeadAcid {
		return ErrNotApplicable
	}
	code := clampRange(laCountsFrommV(delta, 0), 0, 63)
	return d.writeWord(regVEqualizeDelta, uint16(code))
}

// Lead-acid only: writes MAX_CV_TIME (s).
func (d *Device) SetMaxAbsorbTime_s(sec uint16) error {
	if d.chem != ChemLeadAcid {
		return ErrNotApplicable
	}
	return d.writeWord(regMaxAbsorbTime, sec)
}

// Lead-acid only: writes EQUALIZE_TIME (s).
func (d *Device) SetEqualizeTime_s(sec uint16) error {
	if d.chem != ChemLeadAcid {
		return ErrNotApplicable
	}
	return d.writeWord(regEqualizeTime, sec)
}

// Lead-acid only: toggles the LA temp compensation bit in CHARGER_CONFIG_BITS.
func (d *Device) EnableLeadAcidTempComp(on bool) error {
	if d.chem != ChemLeadAcid {
		return ErrNotApplicable
	}

	v, err := d.readWord(regChargerCfgBits)
	if err != nil {
		return err
	}
	if on {
		v |= 1 << 1
	} else {
		v &^= 1 << 1
	}
	return d.writeWord(regChargerCfgBits, v)
}

// ---------------- Low-level SMBus (READ/WRITE WORD) ----------------

// AcknowledgeAlert reads the SMBus Alert Response Address (0x19).
// Returns true if the LTC4015 (0xD1) identified itself and released SMBALERT.
func (d *Device) AcknowledgeAlert() (bool, error) {
	var r [1]byte
	if err := d.i2c.Tx(ARAAddress, nil, r[:]); err != nil {
		return false, err
	}
	return r[0] == 0xD1, nil
}

// Optional functional pin interface (if a platform wants to poll the alert line).
type PinInput func() bool // returns logical level

// SMBALERT is active-low; this helper keeps the driver portable.
func (d *Device) AlertActive(get PinInput) bool { return !get() }

// ---------------- Low-level SMBus (READ/WRITE WORD) ----------------

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

// clampRange limits v to [lo, hi].
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

// qLinear quantises a physical value onto a linear code:
//
//	code_physical = (code + addOne?1:0)*step + offset
//
// so the inverse is:
//
//	code = round((value - offset)/step) - (addOne?1:0)
//
// All parameters are integers in consistent units (e.g. nV or µV).
func qLinear(value, step, offset int64, addOne bool, lo, hi int64) uint16 {
	// round-to-nearest; inputs assumed non-negative in this driver’s use
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

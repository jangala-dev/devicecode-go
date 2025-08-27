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
	v, err := d.ReadConfig()
	if err != nil {
		return err
	}
	return d.WriteConfig(v | mask)
}

func (d *Device) ClearConfigBits(mask ConfigBits) error {
	v, err := d.ReadConfig()
	if err != nil {
		return err
	}
	return d.WriteConfig(v &^ mask)
}

func (d *Device) UpdateConfig(set, clear ConfigBits) error {
	v, err := d.ReadConfig()
	if err != nil {
		return err
	}
	v |= set
	v &^= clear
	return d.WriteConfig(v)
}

// ---------------- Bitmask helpers ----------------

func (b LimitBits) Has(flag LimitBits) bool               { return b&flag != 0 }
func (b ChargerStateBits) Has(flag ChargerStateBits) bool { return b&flag != 0 }
func (b ChargeStatusBits) Has(flag ChargeStatusBits) bool { return b&flag != 0 }
func (s SystemStatus) Has(flag SystemStatus) bool         { return s&flag != 0 }

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

func clamp16(v int64) uint16 {
	if v > 32767 {
		v = 32767
	}
	if v < -32768 {
		v = -32768
	}
	return uint16(int16(v))
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

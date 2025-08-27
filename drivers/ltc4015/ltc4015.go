// Package ltc4015 provides a minimal TinyGo driver for the LTC4015
// multi-chemistry synchronous buck battery charger.
//
// Design notes (datasheet references):
// • I2C/SMBus, 400kHz, read/write word protocol; data-low then data-high
//   (see “I2C TIMING DIAGRAM” and SMBus READ/WRITE WORD protocol).
// • Default 7-bit address = 0b1101000 (from “ADDRESS I2C Address 1101_000[R/W]b”).
// • Telemetry register map & scaling (VBAT, VIN, VSYS, IBAT, IIN, DIE_TEMP, NTC_RATIO, BSR).
// • System status, charger/charge-status alerts, and clear-on-write-0 behaviour.
// • Limit alert enables / limits and Coulomb counter controls.
// • Coulomb counter qLSB formula and QCOUNT registers.

package ltc4015

import (
	"errors"
	"math"

	"tinygo.org/x/drivers"
)

// ---------- Types and configuration ----------

// Chemistry selects scaling where chemistries differ in the datasheet.
type Chemistry uint8

const (
	ChemUnknown  Chemistry = iota
	ChemLithium            // Li-Ion / LiFePO4 use VBAT per-cell factor 192.264µV/LSB.
	ChemLeadAcid           // Lead-acid uses 128.176µV/LSB per cell.
)

// Config holds driver configuration known at compile-/bring-up time.
type Config struct {
	// If zero, AddressDefault is used.
	Address uint16

	// Sense resistors in micro-ohms for battery (RSNSB) and input (RSNSI).
	// Used for integer current scaling (no floats).
	RSNSB_uOhm uint32
	RSNSI_uOhm uint32

	// Cell count. If 0, read from CHEM_CELLS (bits 3:0).
	Cells uint8

	// Chemistry hint; if ChemUnknown, a lithium-style scaling is assumed unless explicitly set by caller.
	// (You can also infer behaviour from board-level configuration if desired.)
	Chem Chemistry

	// Coulomb counter options (see qLSB formula and registers).
	QCountPrescale uint16 // if 0, leave hardware default
}

// Device is the LTC4015 handle.
type Device struct {
	i2c   drivers.I2C
	addr  uint16
	cells uint8
	chem  Chemistry

	rsnsB_uOhm uint32
	rsnsI_uOhm uint32

	// Tx/Rx buffers to avoid per-call allocations (slices made from these).
	w [3]byte
	r [2]byte
}

// New returns a driver instance. Call Configure to finalise setup.
func New(i2c drivers.I2C, cfg Config) *Device {
	addr := cfg.Address
	if addr == 0 {
		addr = AddressDefault
	}
	chem := cfg.Chem
	if chem == ChemUnknown {
		// Default to lithium-style scaling unless caller sets lead-acid explicitly.
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

// Configure finalises initial settings, optionally reads cell count,
// and configures the Coulomb counter if requested.
func (d *Device) Configure(cfg Config) error {
	if cfg.Cells != 0 {
		d.cells = cfg.Cells
	} else {
		// Read CHEM_CELLS to get pins-based cell count (bits 3:0).
		v, err := d.readWord(regChemCells)
		if err == nil {
			d.cells = uint8(v & 0x000F)
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
	// Coulomb counter prescale (CONFIG_BITS en_qcount is handled via EnableQCount()).
	if cfg.QCountPrescale != 0 {
		if err := d.writeWord(regQCountPrescale, cfg.QCountPrescale); err != nil {
			return err
		}
	}
	return nil
}

// ---------- CONFIG_BITS control ----------

// ConfigBits returns the raw CONFIG_BITS register value.
func (d *Device) ConfigBits() (uint16, error) {
	return d.readWord(regConfigBits)
}

// SetConfigBits writes the raw CONFIG_BITS register.
func (d *Device) SetConfigBits(v uint16) error {
	return d.writeWord(regConfigBits, v)
}

// helper to set/clear a single bit in CONFIG_BITS
func (d *Device) setConfigBit(bit int, enable bool) error {
	v, err := d.readWord(regConfigBits)
	if err != nil {
		return err
	}
	if enable {
		v |= 1 << bit
	} else {
		v &^= 1 << bit
	}
	return d.writeWord(regConfigBits, v)
}

// SuspendCharger enables or disables the suspend_charger bit.
func (d *Device) SuspendCharger(enable bool) error {
	return d.setConfigBit(cfgSuspendCharger, enable)
}

// RunBSR triggers a battery series resistance measurement.
// The device clears the bit itself once done.
func (d *Device) RunBSR() error {
	return d.setConfigBit(cfgRunBSR, true)
}

// EnableQCount enables or disables the Coulomb counter.
func (d *Device) EnableQCount(enable bool) error {
	return d.setConfigBit(cfgEnableQCount, enable)
}

// ForceMeasSystemOn keeps measurement system active without VIN.
func (d *Device) ForceMeasSystemOn(enable bool) error {
	return d.setConfigBit(cfgForceMeasSysOn, enable)
}

// EnableMPPTI2C enables or disables MPPT control via I2C.
func (d *Device) EnableMPPTI2C(enable bool) error {
	return d.setConfigBit(cfgMPPTEnableI2C, enable)
}

// ---------- Telemetry (integer units) ----------

// BatteryMilliVPerCell returns VBAT per-cell in millivolts (signed).
// Li chemistries: 192.264µV/LSB per cell; Lead-acid: 128.176µV/LSB per cell.
func (d *Device) BatteryMilliVPerCell() (int32, error) {
	raw, err := d.readWord(regVBAT)
	if err != nil {
		return 0, err
	}
	// Scale to microvolts per cell using integers (avoid float):
	// Li: 192.264µV -> 192264 nV per LSB; Lead: 128176 nV per LSB.
	var nVperLSB int64 = 192264
	if d.chem == ChemLeadAcid {
		nVperLSB = 128176
	}
	uV := (int64(raw) * nVperLSB) / 1000 // convert nV->uV
	return int32(uV / 1000), nil
}

// BatteryMilliVPack returns pack voltage (per-cell * cells) in millivolts.
func (d *Device) BatteryMilliVPack() (int32, error) {
	perCell, err := d.BatteryMilliVPerCell()
	if err != nil {
		return 0, err
	}
	if d.cells == 0 {
		// Fallback if cell pins not read/available.
		return perCell, nil
	}
	return perCell * int32(d.cells), nil
}

// VinMilliV returns VIN in millivolts. 1.648mV/LSB.
func (d *Device) VinMilliV() (int32, error) {
	raw, err := d.readWord(regVIN)
	if err != nil {
		return 0, err
	}
	// 1.648 mV = 1648 µV per LSB.
	uV := int64(raw) * 1648
	return int32(uV / 1000), nil
}

// VsysMilliV returns VSYS in millivolts. 1.648mV/LSB.
func (d *Device) VsysMilliV() (int32, error) {
	raw, err := d.readWord(regVSYS)
	if err != nil {
		return 0, err
	}
	uV := int64(raw) * 1648
	return int32(uV / 1000), nil
}

// IbatMilliA returns battery current in milliamps (signed).
// IBAT LSB is 1.46487µV across RSNSB (two's complement). I[mA]= raw*1.46487µV / RSNSB.
func (d *Device) IbatMilliA() (int32, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, errors.New("RSNSB_uOhm not set")
	}
	raw, err := d.readS16(regIBAT)
	if err != nil {
		return 0, err
	}
	// Use picovolts to keep precision: 1.46487µV = 1,464,870 pV per LSB.
	// I[uA] = raw * 1,464,870 / RSNSB_uOhm  => I[mA] = I[uA]/1000
	uA := (int64(raw) * 1464870) / int64(d.rsnsB_uOhm)
	return int32(uA / 1000), nil
}

// IinMilliA returns input current in milliamps (signed).
// IIN LSB is 1.46487µV across RSNSI.
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

// DieMilliC returns die temperature in milli-°C.
// T[°C] = (DIE_TEMP – 12010)/45.6  ⇒ milli-°C = (raw-12010)*10000/456.
func (d *Device) DieMilliC() (int32, error) {
	raw, err := d.readS16(regDieTemp)
	if err != nil {
		return 0, err
	}
	mC := (int64(raw) - 12010) * 10000 / 456
	return int32(mC), nil
}

// BSRMicroOhmPerCell returns per-cell battery series resistance in micro-ohms.
// Lithium: Ω/cell = (BSR/500)*RSNSB; Lead-acid: (BSR/750)*RSNSB.
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
	uOhm := (int64(raw) * int64(d.rsnsB_uOhm)) / div
	return uint32(uOhm), nil
}

// MeasSystemValid reports whether telemetry is ready (MEAS_SYS_VALID bit0).
func (d *Device) MeasSystemValid() (bool, error) {
	v, err := d.readWord(regMeasSysValid)
	if err != nil {
		return false, err
	}
	return (v & 0x0001) != 0, nil
}

// SystemStatus returns the raw SYSTEM_STATUS register (0x39).
func (d *Device) SystemStatus() (uint16, error) { return d.readWord(regSystemStatus) }

// ---------- Alerts & limits: user-friendly setters ----------

// VBAT limits are per-cell (same format as VBAT). Lithium 192.264µV/LSB; Lead-acid 128.176µV/LSB.
func (d *Device) SetVBATWindow_mVPerCell(lo_mV, hi_mV int32) error {
	if err := d.writeWord(regVBATLoAlertLimit, d.toVBATCode(lo_mV)); err != nil {
		return err
	}
	return d.writeWord(regVBATHiAlertLimit, d.toVBATCode(hi_mV))
}

// VIN/VSYS limits use 1.648mV per LSB.
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

// IIN_HI in mA (1.46487µV/RSNSI per LSB).
func (d *Device) SetIINHigh_mA(mA int32) error {
	if d.rsnsI_uOhm == 0 {
		return errors.New("RSNSI_uOhm not set")
	}
	code := d.currCode(mA, d.rsnsI_uOhm)
	return d.writeWord(regIINHiAlertLimit, code)
}

// IBAT_LO in mA (alert when charge current falls below threshold).
func (d *Device) SetIBATLow_mA(mA int32) error {
	if d.rsnsB_uOhm == 0 {
		return errors.New("RSNSB_uOhm not set")
	}
	code := d.currCode(mA, d.rsnsB_uOhm)
	return d.writeWord(regIBATLoAlertLimit, code)
}

// Die temperature high limit in milli-°C. raw = 12010 + 45.6*°C.
func (d *Device) SetDieTempHigh_mC(mC int32) error {
	raw := int64(12010) + (int64(456)*int64(mC))/10000
	return d.writeWord(regDieTempHiAlertLimit, clamp16(raw))
}

// BSR high limit in µΩ per cell. Lithium uses /500, lead-acid /750.
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

// NTC ratio limits in raw code units (left raw to avoid board-dependent thermistor maths).
func (d *Device) SetNTCRatioWindowRaw(hi, lo uint16) error {
	if err := d.writeWord(regNTCRatioHiAlertLimit, hi); err != nil {
		return err
	}
	return d.writeWord(regNTCRatioLoAlertLimit, lo)
}

// ---------- Alerts: enable (typed), read, clear ----------

func (d *Device) EnableLimitAlertsMask(mask LimitEnable) error {
	return d.writeWord(regEnLimitAlerts, uint16(mask))
}
func (d *Device) EnableChargerStateAlertsMask(mask ChargerStateEnable) error {
	return d.writeWord(regEnChargerStAlerts, uint16(mask))
}
func (d *Device) EnableChargeStatusAlertsMask(mask ChargeStatusEnable) error {
	return d.writeWord(regEnChargeStAlerts, uint16(mask))
}

// Read/Clear R/Clear alert registers. Writing 0 clears asserted bits.
func (d *Device) ReadLimitAlerts() (uint16, error)        { return d.readWord(regLimitAlerts) }
func (d *Device) ReadChargerStateAlerts() (uint16, error) { return d.readWord(regChargerStateAlert) }
func (d *Device) ReadChargeStatusAlerts() (uint16, error) { return d.readWord(regChargeStatAlerts) }

func (d *Device) ClearLimitAlerts() error        { return d.writeWord(regLimitAlerts, 0x0000) }
func (d *Device) ClearChargerStateAlerts() error { return d.writeWord(regChargerStateAlert, 0x0000) }
func (d *Device) ClearChargeStatusAlerts() error { return d.writeWord(regChargeStatAlerts, 0x0000) }

// High-level convenience to set limits then enable chosen alert classes.
type AlertLimits struct {
	// Per-cell VBAT window (mV/cell). If both are zero, VBAT limits are not set.
	VBATLo_mVPerCell int32
	VBATHi_mVPerCell int32

	// VIN/VSYS windows (mV). If both are zero, window is not set.
	VINLo_mV, VINHi_mV   int32
	VSYSLo_mV, VSYSHi_mV int32

	// Currents (mA). Zero leaves the limit unchanged.
	IINHi_mA  int32
	IBATLo_mA int32

	// Die temperature high limit (m°C). Zero leaves unchanged.
	DieTempHi_mC int32

	// Battery series resistance high limit (µΩ/cell). Zero leaves unchanged.
	BSRHi_uOhmPerCell uint32

	// NTC ratio raw window; both zero leaves unchanged.
	NTCRatioHiRaw, NTCRatioLoRaw uint16

	// Enable masks to apply after limits are written (0 leaves disabled).
	EnableLimits       LimitEnable
	EnableChargerState ChargerStateEnable
	EnableChargeStatus ChargeStatusEnable
}

func (d *Device) ConfigureAlerts(a AlertLimits) error {
	// Write only if non-zero (keeps semantics simple and allocation-free).
	if a.VBATLo_mVPerCell|a.VBATHi_mVPerCell != 0 {
		if err := d.SetVBATWindow_mVPerCell(a.VBATLo_mVPerCell, a.VBATHi_mVPerCell); err != nil {
			return err
		}
	}
	if a.VINLo_mV|a.VINHi_mV != 0 {
		if err := d.SetVINWindow_mV(a.VINLo_mV, a.VINHi_mV); err != nil {
			return err
		}
	}
	if a.VSYSLo_mV|a.VSYSHi_mV != 0 {
		if err := d.SetVSYSWindow_mV(a.VSYSLo_mV, a.VSYSHi_mV); err != nil {
			return err
		}
	}
	if a.IINHi_mA != 0 {
		if err := d.SetIINHigh_mA(a.IINHi_mA); err != nil {
			return err
		}
	}
	if a.IBATLo_mA != 0 {
		if err := d.SetIBATLow_mA(a.IBATLo_mA); err != nil {
			return err
		}
	}
	if a.DieTempHi_mC != 0 {
		if err := d.SetDieTempHigh_mC(a.DieTempHi_mC); err != nil {
			return err
		}
	}
	if a.BSRHi_uOhmPerCell != 0 {
		if err := d.SetBSRHigh_uOhmPerCell(a.BSRHi_uOhmPerCell); err != nil {
			return err
		}
	}
	if a.NTCRatioHiRaw|a.NTCRatioLoRaw != 0 {
		if err := d.SetNTCRatioWindowRaw(a.NTCRatioHiRaw, a.NTCRatioLoRaw); err != nil {
			return err
		}
	}
	// Now enable masks (0 leaves them disabled).
	if a.EnableLimits != 0 {
		if err := d.EnableLimitAlertsMask(a.EnableLimits); err != nil {
			return err
		}
	}
	if a.EnableChargerState != 0 {
		if err := d.EnableChargerStateAlertsMask(a.EnableChargerState); err != nil {
			return err
		}
	}
	if a.EnableChargeStatus != 0 {
		if err := d.EnableChargeStatusAlertsMask(a.EnableChargeStatus); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Coulomb counter ----------

// SetQCount sets the QCOUNT accumulator (0x13).
func (d *Device) SetQCount(v uint16) error { return d.writeWord(regQCount, v) }

// QCount reads the QCOUNT accumulator (0x13).
func (d *Device) QCount() (uint16, error) { return d.readWord(regQCount) }

// SetQCountLimits sets QCOUNT_LO/H I alert limits (0x10/0x11).
func (d *Device) SetQCountLimits(lo, hi uint16) error {
	if err := d.writeWord(regQCountLoLimit, lo); err != nil {
		return err
	}
	return d.writeWord(regQCountHiLimit, hi)
}

// SetQCountPrescale sets QCOUNT_PRESCALE_FACTOR (0x12).
func (d *Device) SetQCountPrescale(p uint16) error { return d.writeWord(regQCountPrescale, p) }

// qCountLSBnC returns qLSB in nanoCoulombs per tick.
// This avoids floats: nanoCoulombs = 1e-9 C.
func (d *Device) qCountLSBnC(prescale uint16) (uint64, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, errors.New("RSNSB_uOhm not set")
	}
	// KQC = 8333.33 Hz/V ≈ 833333/100 (scaled to avoid float).
	const kqc_num = 833333
	const kqc_den = 100

	// RSNS in micro-ohm → ohm = RSNS / 1e6.
	// Formula: qLSB = prescale / (KQC * RSNS).
	// Scale into nC: qLSB[nC] = (prescale * 1e9) / (KQC * RSNS[Ω]).
	num := uint64(prescale) * 1_000_000_000 * kqc_den
	den := uint64(kqc_num) * uint64(d.rsnsB_uOhm)
	return num / den, nil
}

// CoulombsDelta converts a delta-QCOUNT into Coulombs (integer, in µC).
func (d *Device) CoulombsDelta(deltaQ int32, prescale uint16) (int64, error) {
	qlsb, err := d.qCountLSBnC(prescale) // in nC
	if err != nil {
		return 0, err
	}
	// deltaQ * qlsb[nC] → nC. Divide by 1000 → µC.
	return (int64(deltaQ) * int64(qlsb)) / 1000, nil
}

// MilliAmpHoursDelta converts a delta-QCOUNT into mAh.
// mAh = (Coulombs / 3600) * 1000.
func (d *Device) MilliAmpHoursDelta(deltaQ int32, prescale uint16) (int32, error) {
	qlsb, err := d.qCountLSBnC(prescale) // nC
	if err != nil {
		return 0, err
	}
	// ΔQ[nC] = deltaQ * qlsb.
	nC := int64(deltaQ) * int64(qlsb)
	// Convert: 1 mAh = 3.6 C = 3.6e9 nC.
	mAh := nC / 3_600_000_000
	return int32(mAh), nil
}

// ---------- Low-level SMBus read/write word (little-endian: LOW then HIGH) ----------

func (d *Device) readWord(reg byte) (uint16, error) {
	d.w[0] = reg
	if err := d.i2c.Tx(d.addr, d.w[:1], d.r[:2]); err != nil {
		return 0, err
	}
	// low then high per SMBus READ WORD.
	return uint16(d.r[0]) | uint16(d.r[1])<<8, nil
}

func (d *Device) readS16(reg byte) (int16, error) {
	u, err := d.readWord(reg)
	return int16(u), err
}

func (d *Device) writeWord(reg byte, val uint16) error {
	d.w[0] = reg
	d.w[1] = byte(val & 0xFF)        // low byte
	d.w[2] = byte((val >> 8) & 0xFF) // high byte
	return d.i2c.Tx(d.addr, d.w[:3], nil)
}

// ---------- Private scaling helpers (integer arithmetic only) ----------

// clamp16 bounds an int64 into the signed 16-bit range
// and returns the encoded uint16 form.
func clamp16(v int64) uint16 {
	if v > math.MaxInt16 {
		v = math.MaxInt16
	}
	if v < math.MinInt16 {
		v = math.MinInt16
	}
	return uint16(int16(v))
}

// toVBATCode converts millivolts-per-cell into the raw register code.
// Li: 192.264µV/LSB; Lead-acid: 128.176µV/LSB.
func (d *Device) toVBATCode(mV int32) uint16 {
	nVperLSB := int64(192264)
	if d.chem == ChemLeadAcid {
		nVperLSB = 128176
	}
	code := (int64(mV)*1_000_000 + nVperLSB/2) / nVperLSB
	return clamp16(code)
}

// toCode_1p648mV_LSB converts a millivolt value into raw code units (1.648mV/LSB).
func (d *Device) toCode_1p648mV_LSB(mV int32) uint16 {
	const nVperLSB = 1_648_000
	code := (int64(mV)*1_000_000 + nVperLSB/2) / nVperLSB
	return clamp16(code)
}

// currCode converts a current threshold in mA into the raw register code
// given a sense resistor value in micro-ohms.
func (d *Device) currCode(mA int32, rsns_uOhm uint32) uint16 {
	const pVperLSB = 1_464_870
	uA := int64(mA) * 1000
	code := (uA*int64(rsns_uOhm) + pVperLSB/2) / pVperLSB
	return clamp16(code)
}

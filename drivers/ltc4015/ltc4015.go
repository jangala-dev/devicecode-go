// Package ltc4015 provides a minimal TinyGo driver for the LTC4015
// multi-chemistry synchronous buck battery charger.
//
// Design notes (datasheet references):
// • I2C/SMBus, 400kHz, read/write word protocol; data-low then data-high
//   (see “I2C TIMING DIAGRAM” and SMBus READ/WRITE WORD protocol). :contentReference[oaicite:0]{index=0}
// • Default 7-bit address = 0b1101000 (from “ADDRESS I2C Address 1101_000[R/W]b”). :contentReference[oaicite:1]{index=1}
// • Telemetry register map & scaling (VBAT, VIN, VSYS, IBAT, IIN, DIE_TEMP, NTC_RATIO, BSR). :contentReference[oaicite:2]{index=2}
// • System status, charger/charge-status alerts, and clear-on-write-0 behaviour. :contentReference[oaicite:3]{index=3}
// • Limit alert enables / limits and Coulomb counter controls. :contentReference[oaicite:4]{index=4}
// • Coulomb counter qLSB formula and QCOUNT registers. :contentReference[oaicite:5]{index=5}

package ltc4015

import (
	"errors"

	"tinygo.org/x/drivers"
)

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
		// Read CHEM_CELLS to get pins-based cell count (bits 3:0). :contentReference[oaicite:10]{index=10}
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
	// Coulomb counter prescale and enable (CONFIG_BITS bit2 en_qcount). :contentReference[oaicite:11]{index=11}
	if cfg.QCountPrescale != 0 {
		if err := d.writeWord(regQCountPrescale, cfg.QCountPrescale); err != nil {
			return err
		}
	}
	return nil
}

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
// Li chemistries: 192.264µV/LSB per cell; Lead-acid: 128.176µV/LSB per cell. :contentReference[oaicite:12]{index=12}
func (d *Device) BatteryMilliVPerCell() (int32, error) {
	raw, err := d.readS16(regVBAT)
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
	// millivolts:
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

// VinMilliV returns VIN in millivolts. 1.648mV/LSB. :contentReference[oaicite:13]{index=13}
func (d *Device) VinMilliV() (int32, error) {
	raw, err := d.readS16(regVIN)
	if err != nil {
		return 0, err
	}
	// 1.648 mV = 1648 µV per LSB.
	uV := int64(raw) * 1648
	return int32(uV / 1000), nil
}

// VsysMilliV returns VSYS in millivolts. 1.648mV/LSB. :contentReference[oaicite:14]{index=14}
func (d *Device) VsysMilliV() (int32, error) {
	raw, err := d.readS16(regVSYS)
	if err != nil {
		return 0, err
	}
	uV := int64(raw) * 1648
	return int32(uV / 1000), nil
}

// IbatMilliA returns battery current in milliamps (signed).
// IBAT LSB is 1.46487µV across RSNSB (two's complement). I[mA]= raw*1.46487µV / RSNSB. :contentReference[oaicite:15]{index=15}
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
// IIN LSB is 1.46487µV across RSNSI. :contentReference[oaicite:16]{index=16}
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
// T[°C] = (DIE_TEMP – 12010)/45.6  ⇒ milli-°C = (raw-12010)*10000/456. :contentReference[oaicite:17]{index=17}
func (d *Device) DieMilliC() (int32, error) {
	raw, err := d.readS16(regDieTemp)
	if err != nil {
		return 0, err
	}
	mC := (int64(raw) - 12010) * 10000 / 456
	return int32(mC), nil
}

// BSRMicroOhmPerCell returns per-cell battery series resistance in micro-ohms.
// Lithium: Ω/cell = (BSR/500)*RSNSB; Lead-acid: (BSR/750)*RSNSB. :contentReference[oaicite:18]{index=18}
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
	if uOhm < 0 {
		uOhm = 0
	}
	return uint32(uOhm), nil
}

// MeasSystemValid reports whether telemetry is ready (MEAS_SYS_VALID bit0). :contentReference[oaicite:19]{index=19}
func (d *Device) MeasSystemValid() (bool, error) {
	v, err := d.readWord(regMeasSysValid)
	if err != nil {
		return false, err
	}
	return (v & 0x0001) != 0, nil
}

// SystemStatus returns the raw SYSTEM_STATUS register (0x39). :contentReference[oaicite:20]{index=20}
func (d *Device) SystemStatus() (uint16, error) { return d.readWord(regSystemStatus) }

// ---------- Alerts (enable, read, clear) ----------

// EnableLimitAlerts writes EN_LIMIT_ALERTS (0x0D). Set bits per datasheet table. :contentReference[oaicite:21]{index=21}
func (d *Device) EnableLimitAlerts(mask uint16) error { return d.writeWord(regEnLimitAlerts, mask) }

// EnableChargerStateAlerts writes EN_CHARGER_STATE_ALERTS (0x0E). :contentReference[oaicite:22]{index=22}
func (d *Device) EnableChargerStateAlerts(mask uint16) error {
	return d.writeWord(regEnChargerStAlerts, mask)
}

// EnableChargeStatusAlerts writes EN_CHARGE_STATUS_ALERTS (0x0F). :contentReference[oaicite:23]{index=23}
func (d *Device) EnableChargeStatusAlerts(mask uint16) error {
	return d.writeWord(regEnChargeStAlerts, mask)
}

// Read/Clear R/Clear alert registers. Writing 0 clears asserted bits. :contentReference[oaicite:24]{index=24}
func (d *Device) ReadLimitAlerts() (uint16, error)        { return d.readWord(regLimitAlerts) }
func (d *Device) ReadChargerStateAlerts() (uint16, error) { return d.readWord(regChargerStateAlert) }
func (d *Device) ReadChargeStatusAlerts() (uint16, error) { return d.readWord(regChargeStatAlerts) }

func (d *Device) ClearLimitAlerts() error        { return d.writeWord(regLimitAlerts, 0x0000) }
func (d *Device) ClearChargerStateAlerts() error { return d.writeWord(regChargerStateAlert, 0x0000) }
func (d *Device) ClearChargeStatusAlerts() error { return d.writeWord(regChargeStatAlerts, 0x0000) }

// ---------- Coulomb counter ----------

// SetQCount sets the QCOUNT accumulator (0x13). :contentReference[oaicite:25]{index=25}
func (d *Device) SetQCount(v uint16) error { return d.writeWord(regQCount, v) }

// QCount reads the QCOUNT accumulator (0x13). :contentReference[oaicite:26]{index=26}
func (d *Device) QCount() (uint16, error) { return d.readWord(regQCount) }

// SetQCountLimits sets QCOUNT_LO/H I alert limits (0x10/0x11). :contentReference[oaicite:27]{index=27}
func (d *Device) SetQCountLimits(lo, hi uint16) error {
	if err := d.writeWord(regQCountLoLimit, lo); err != nil {
		return err
	}
	return d.writeWord(regQCountHiLimit, hi)
}

// SetQCountPrescale sets QCOUNT_PRESCALE_FACTOR (0x12). :contentReference[oaicite:28]{index=28}
func (d *Device) SetQCountPrescale(p uint16) error { return d.writeWord(regQCountPrescale, p) }

// ---------- Low-level SMBus read/write word (little-endian: LOW then HIGH) ----------

func (d *Device) readWord(reg byte) (uint16, error) {
	d.w[0] = reg
	if err := d.i2c.Tx(d.addr, d.w[:1], d.r[:2]); err != nil {
		return 0, err
	}
	// low then high per SMBus READ WORD. :contentReference[oaicite:29]{index=29}
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

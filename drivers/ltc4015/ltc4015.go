// Package ltc4015 provides a TinyGo driver for the LTC4015
// multi-chemistry battery charger controller with I2C interface.
package ltc4015

import (
	"errors"

	"tinygo.org/x/drivers"
)

var (
	ErrTx    = errors.New("i2c transaction failed")
	ErrAlert = errors.New("alert active")
)

// Device represents a connection to an LTC4015 device over I2C.
type Device struct {
	bus     drivers.I2C
	Address uint16

	// Measurement scaling configuration
	rsnsb   int64 // battery sense resistor in µΩ
	rsnsi   int64 // input sense resistor in µΩ
	lithium bool
	cells   int

	// External resistor values for scaling (in mΩ for divider ratios)
	rntc  int64
	rbsrt int64
	rbsrb int64
	rvin1 int64
	rvin2 int64

	// Reusable buffers
	wbuf [3]byte
	rbuf [2]byte
	abuf [1]byte
}

// Config contains user-defined parameters for the LTC4015 device.
type Config struct {
	RSNSB   float32 // Battery sense resistor in ohms
	RSNSI   float32 // Input sense resistor in ohms
	Lithium bool    // true for lithium-ion, false for lead-acid
	Cells   int     // Cell count (lead-acid only)

	RNTC  float32 // Thermistor reference resistor in ohms
	RBSRT float32 // BSR divider top resistor in ohms
	RBSRB float32 // BSR divider bottom resistor in ohms
	RVIN1 float32 // VIN divider top resistor in ohms
	RVIN2 float32 // VIN divider bottom resistor in ohms

	Address  uint16 // I2C address (defaults to 0x68)
	Features uint16 // Initial CONFIG_BITS mask
}

// DefaultConfig provides safe defaults based on a typical demo board.
var DefaultConfig = Config{
	RSNSB:   0.01,
	RSNSI:   0.01,
	Lithium: true,
	Cells:   1,

	RNTC:  10000.0,
	RBSRT: 100000.0,
	RBSRB: 10000.0,
	RVIN1: 100000.0,
	RVIN2: 10000.0,

	Address:  DeviceAddress,
	Features: 0,
}

// New returns a new Device instance.
func New(bus drivers.I2C) Device {
	return Device{
		bus:     bus,
		Address: DeviceAddress,
	}
}

// Configure applies the provided configuration.
func (d *Device) Configure(cfg Config) error {
	d.rsnsb = int64(cfg.RSNSB * 1e6) // Ω → µΩ
	d.rsnsi = int64(cfg.RSNSI * 1e6)
	d.lithium = cfg.Lithium
	d.cells = cfg.Cells

	d.rntc = int64(cfg.RNTC * 1000)   // Ω → mΩ
	d.rbsrt = int64(cfg.RBSRT * 1000) // Ω → mΩ
	d.rbsrb = int64(cfg.RBSRB * 1000)
	d.rvin1 = int64(cfg.RVIN1 * 1000)
	d.rvin2 = int64(cfg.RVIN2 * 1000)

	if cfg.Address != 0 {
		d.Address = cfg.Address
	}
	if cfg.Features != 0 {
		if err := d.writeRegister(REG_CONFIG_BITS, cfg.Features); err != nil {
			return err
		}
	}
	return nil
}

func (d *Device) readRegisterSigned(reg byte) (int16, error) {
	u, err := d.readRegister(reg)
	return int16(u), err
}

// ------------------
// I2C (internal)
// ------------------

func (d *Device) readRegister(reg byte) (uint16, error) {
	d.wbuf[0] = reg
	err := d.bus.Tx(d.Address, d.wbuf[:1], d.rbuf[:])
	if err != nil {
		return 0, ErrTx
	}
	return uint16(d.rbuf[1])<<8 | uint16(d.rbuf[0]), nil
}

func (d *Device) writeRegister(reg byte, value uint16) error {
	d.wbuf[0] = reg
	d.wbuf[1] = byte(value & 0xFF)
	d.wbuf[2] = byte((value >> 8) & 0xFF)
	return d.bus.Tx(d.Address, d.wbuf[:3], nil)
}

func (d *Device) isBitSet(reg byte, mask uint16) (bool, error) {
	val, err := d.readRegister(reg)
	if err != nil {
		return false, err
	}
	return val&mask != 0, nil
}

// ------------------
// Alerts
// ------------------

func (d *Device) AlertMeasSysValid() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_MEAS_SYS_VALID)
}
func (d *Device) AlertQCountLow() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_QCOUNT_LOW)
}
func (d *Device) AlertQCountHigh() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_QCOUNT_HIGH)
}
func (d *Device) AlertVBATLow() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_VBAT_LOW)
}
func (d *Device) AlertVBATHigh() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_VBAT_HIGH)
}
func (d *Device) AlertVINLow() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_VIN_LOW)
}
func (d *Device) AlertVINHigh() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_VIN_HIGH)
}
func (d *Device) AlertVSYSLow() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_VSYS_LOW)
}
func (d *Device) AlertVSYSHigh() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_VSYS_HIGH)
}
func (d *Device) AlertIINHigh() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_IIN_HIGH)
}
func (d *Device) AlertIBATLow() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_IBAT_LOW)
}
func (d *Device) AlertTempHigh() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_TEMP_HIGH)
}
func (d *Device) AlertBSRHigh() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_BSR_HIGH)
}
func (d *Device) AlertNTCRatioHigh() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_NTC_RATIO_HIGH)
}
func (d *Device) AlertNTCRatioLow() (bool, error) {
	return d.isBitSet(REG_LIMIT_ALERT_STATUS, ALERT_NTC_RATIO_LOW)
}

// ------------------
// Charger States (live, REG_CHARGER_STATE = 0x34)
// ------------------

func (d *Device) StateEqualize() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_EQUALIZE)
}
func (d *Device) StateAbsorb() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_ABSORB)
}
func (d *Device) StateSuspend() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_SUSPEND)
}
func (d *Device) StatePrecharge() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_PRECHARGE)
}
func (d *Device) StateCCCV() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_CC_CV)
}
func (d *Device) StateNTCPause() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_NTC_PAUSE)
}
func (d *Device) StateTimerTerm() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_TIMER_TERM)
}
func (d *Device) StateCOverXTerm() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_C_OVER_X_TERM)
}
func (d *Device) StateMaxChargeFault() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_MAX_CHARGE_FAULT)
}
func (d *Device) StateBatteryMissing() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_BAT_MISSING)
}
func (d *Device) StateBatteryShort() (bool, error) {
	return d.isBitSet(REG_CHARGER_STATE, STATE_BAT_SHORT)
}

// ------------------
// Charger Status (live, REG_CHARGE_STATUS = 0x35)
// ------------------

func (d *Device) StatusUVCLActive() (bool, error) {
	return d.isBitSet(REG_CHARGE_STATUS, STATUS_UVCL_ACTIVE)
}
func (d *Device) StatusIINLimit() (bool, error) {
	return d.isBitSet(REG_CHARGE_STATUS, STATUS_IIN_LIMIT)
}
func (d *Device) StatusCCActive() (bool, error) {
	return d.isBitSet(REG_CHARGE_STATUS, STATUS_CC_ACTIVE)
}
func (d *Device) StatusCVActive() (bool, error) {
	return d.isBitSet(REG_CHARGE_STATUS, STATUS_CV_ACTIVE)
}

// ------------------
// System Status
// ------------------

func (d *Device) SysChargerEnabled() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_CHARGER_ENABLED)
}
func (d *Device) SysMPPTEnabled() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_MPPT_EN_PIN)
}
func (d *Device) SysEqualizeRequested() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_EQUALIZE_REQUESTED)
}
func (d *Device) SysDRVCCGood() (bool, error) { return d.isBitSet(REG_SYSTEM_STATUS, SYS_DRVCC_GOOD) }
func (d *Device) SysCellCountError() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_CELL_COUNT_ERROR)
}
func (d *Device) SysOkToCharge() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_OK_TO_CHARGE)
}
func (d *Device) SysNoRT() (bool, error) { return d.isBitSet(REG_SYSTEM_STATUS, SYS_NO_RT) }
func (d *Device) SysThermalShutdown() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_THERMAL_SHUTDOWN)
}
func (d *Device) SysVINOVLO() (bool, error) { return d.isBitSet(REG_SYSTEM_STATUS, SYS_VIN_OVLO) }
func (d *Device) SysVINGreaterVBAT() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_VIN_GT_VBAT)
}
func (d *Device) SysINTVCCGT4V3() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_INTVCC_GT_4V3)
}
func (d *Device) SysINTVCCGT2V8() (bool, error) {
	return d.isBitSet(REG_SYSTEM_STATUS, SYS_INTVCC_GT_2V8)
}

// ------------------
// Engineering Values (fixed-point arithmetic)
// ------------------

// ReadVBAT returns the battery voltage in millivolts.
func (d *Device) ReadVBAT() (int32, error) {
	u, err := d.readRegister(REG_VBAT)
	if err != nil {
		return 0, err
	}
	if d.cells < MIN_CELLS {
		d.cells = MIN_CELLS
	}
	lsb := int64(LSB_VBAT_LI_uV)
	if !d.lithium {
		lsb = int64(LSB_VBAT_LA_uV)
	}
	uV := int64(u) * lsb * int64(d.cells)
	return int32(uV / 1_000_000), nil
}

// ReadVIN returns the input voltage in millivolts.
func (d *Device) ReadVIN() (int32, error) {
	val, err := d.readRegister(REG_VIN)
	if err != nil {
		return 0, err
	}
	uV := int64(val) * LSB_VIN_uV
	return int32(uV / 1000), nil
}

// ReadVSYS returns the system voltage in millivolts.
func (d *Device) ReadVSYS() (int32, error) {
	val, err := d.readRegister(REG_VSYS)
	if err != nil {
		return 0, err
	}
	uV := int64(val) * LSB_VSYS_uV
	return int32(uV / 1000), nil
}

// ReadIBAT returns the battery current in milliamps.
func (d *Device) ReadIBAT() (int32, error) {
	raw, err := d.readRegisterSigned(REG_IBAT)
	if err != nil {
		return 0, err
	}
	if d.rsnsb == 0 {
		return 0, ErrTx
	}
	// µA = raw * 1,464,870 / RSNSB(µΩ)
	uA := int64(raw) * LSB_CURR_nA * 1000 / d.rsnsb
	return int32(uA / 1_000), nil
}

// ReadIIN returns the input current in milliamps.
func (d *Device) ReadIIN() (int32, error) {
	raw, err := d.readRegisterSigned(REG_IIN)
	if err != nil {
		return 0, err
	}
	if d.rsnsi == 0 {
		return 0, ErrTx
	}
	uA := int64(raw) * LSB_CURR_nA * 1000 / d.rsnsi
	return int32(uA / 1_000), nil
}

// ReadDieTemp returns die temperature in tenths of °C (×0.1 °C).
func (d *Device) ReadDieTemp() (int32, error) {
	val, err := d.readRegister(REG_DIETEMP)
	if err != nil {
		return 0, err
	}
	// tenths = 10*(val - OFFSET)/45.6  → integer-safe form: (val - OFFSET)*100/456
	tenths := (int64(val) - TEMP_OFFSET) * TEMP_SCALE_FACTOR / TEMP_SCALE_X10
	return int32(tenths), nil
}

// ReadNTCRatio returns the NTC ratio as a percentage (0–100).
func (d *Device) ReadNTCRatio() (int32, error) {
	u, err := d.readRegister(REG_NTC_RATIO)
	if err != nil {
		return 0, err
	}
	return int32(int64(u) * NTC_RATIO_SCALE_PCT / NTC_RATIO_DEN), nil
}

// ReadBSRResistance returns battery series resistance in micro-ohms (µΩ).
func (d *Device) ReadBSRResistance() (int32, error) {
	val, err := d.readRegister(REG_BSR)
	if err != nil {
		return 0, err
	}
	if d.cells < 1 {
		d.cells = 1
	}
	den := int64(BSR_DEN_LI)
	if !d.lithium {
		den = BSR_DEN_LA
	}
	// per-cell Ω = (val/den)*RSNSB; return total µΩ
	μΩperCell := int64(val) * d.rsnsb / den
	total := μΩperCell * int64(d.cells)
	return int32(total), nil
}

// ReadCoulombCount returns accumulated charge in mAh.
func (d *Device) ReadCoulombCount() (int32, error) {
	qc, err := d.readRegister(REG_QCOUNT)
	if err != nil {
		return 0, err
	}
	ps, err := d.readRegister(REG_QCOUNT_PRESCALE)
	if err != nil {
		return 0, err
	}
	if d.rsnsb == 0 {
		return 0, ErrTx
	}
	// mAh = QCOUNT * PRESCALE * 1e9 / (8333.33*3600 * RSNSB(µΩ))
	// 8333.33*3600 ≈ 29,999,988 (per datasheet 8333.33)
	num := int64(qc) * int64(ps) * QCOUNT_NUM_SCALE
	den := int64(d.rsnsb) * QCOUNT_DEN_CONST
	return int32(num / den), nil
}

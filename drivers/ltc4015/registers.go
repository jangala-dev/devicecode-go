// Package ltc4015 provides constants for register addresses and bitfields used
// in the operation of the LTC4015 battery charger controller.
package ltc4015

// Device I2C address and alert response
const (
	DeviceAddress     = 0x68
	AlertResponseAddr = 0x0C
)

//
// ========== Register Addresses ==========
//

// Measurement (ADC)
const (
	REG_VBAT           = 0x3A
	REG_VIN            = 0x3B
	REG_VSYS           = 0x3C
	REG_IBAT           = 0x3D
	REG_IIN            = 0x3E
	REG_DIETEMP        = 0x3F
	REG_NTC_RATIO      = 0x40
	REG_BSR            = 0x41
	REG_VBAT_FILT      = 0x47
	REG_ICHARGE_BSR    = 0x48
	REG_MEAS_SYS_VALID = 0x4A
)

// Charger state and system status
const (
	REG_CHARGER_STATE = 0x34
	REG_CHARGE_STATUS = 0x35
	REG_SYSTEM_STATUS = 0x39
	REG_JEITA_REGION  = 0x42
	REG_CHEM_CELLS    = 0x43
	REG_ICHARGE_DAC   = 0x44
	REG_VCHARGE_DAC   = 0x45
	REG_IIN_LIMIT_DAC = 0x46
)

// Alert enable registers
const (
	REG_LIMIT_ALERT_ENABLE    = 0x0D
	REG_CHARGER_STATE_ENABLE  = 0x0E
	REG_CHARGER_STATUS_ENABLE = 0x0F
)

// Alert status registers
const (
	REG_LIMIT_ALERT_STATUS    = 0x36
	REG_CHARGER_STATE_STATUS  = 0x37
	REG_CHARGER_STATUS_STATUS = 0x38
)

// Alert threshold limits
const (
	REG_VBAT_LO_ALERT_LIMIT      = 0x01
	REG_VBAT_HI_ALERT_LIMIT      = 0x02
	REG_VIN_LO_ALERT_LIMIT       = 0x03
	REG_VIN_HI_ALERT_LIMIT       = 0x04
	REG_VSYS_LO_ALERT_LIMIT      = 0x05
	REG_VSYS_HI_ALERT_LIMIT      = 0x06
	REG_IIN_HI_ALERT_LIMIT       = 0x07
	REG_IBAT_LO_ALERT_LIMIT      = 0x08
	REG_DIE_TEMP_HI_ALERT_LIMIT  = 0x09
	REG_BSR_HI_ALERT_LIMIT       = 0x0A
	REG_NTC_RATIO_HI_ALERT_LIMIT = 0x0B
	REG_NTC_RATIO_LO_ALERT_LIMIT = 0x0C

	REG_QCOUNT_LO_ALERT_LIMIT = 0x10
	REG_QCOUNT_HI_ALERT_LIMIT = 0x11
)

// Coulomb counter
const (
	REG_QCOUNT          = 0x13
	REG_QCOUNT_PRESCALE = 0x12
)

// Configuration and charging control
const (
	REG_CONFIG_BITS        = 0x14
	REG_IIN_LIMIT_SETTING  = 0x15
	REG_VIN_UVCL_SETTING   = 0x16
	REG_ARM_SHIP_MODE      = 0x19
	REG_ICHARGE_TARGET     = 0x1A
	REG_VCHARGE_SETTING    = 0x1B
	REG_C_OVER_X_THRESHOLD = 0x1C
	REG_MAX_CV_TIME        = 0x1D
	REG_MAX_CHARGE_TIME    = 0x1E
	REG_MAX_CHARGE_TIMER   = 0x30
	REG_CV_TIMER           = 0x31
	REG_ABSORB_TIMER       = 0x32
	REG_EQUALIZE_TIMER     = 0x33
)

// JEITA configuration
const (
	REG_JEITA_T1                   = 0x1F
	REG_JEITA_T2                   = 0x20
	REG_JEITA_T3                   = 0x21
	REG_JEITA_T4                   = 0x22
	REG_JEITA_T5                   = 0x23
	REG_JEITA_T6                   = 0x24
	REG_VCHARGE_JEITA_6_5          = 0x25
	REG_VCHARGE_JEITA_4_3_2        = 0x26
	REG_ICHARGE_JEITA_6_5          = 0x27
	REG_ICHARGE_JEITA_4_3_2        = 0x28
	REG_CHARGER_CONFIG_BITS        = 0x29
	REG_VABSORB_DELTA              = 0x2A
	REG_MAX_ABSORB_TIME            = 0x2B
	REG_VEQUALIZE_DELTA            = 0x2C
	REG_EQUALIZE_TIME              = 0x2D
	REG_LIFEPO4_RECHARGE_THRESHOLD = 0x2E
)

// Alert bit masks
// REG_LIMIT_ALERT_ENABLE and REG_LIMIT_ALERT_STATUS
const (
	ALERT_MEAS_SYS_VALID = 1 << 15
	ALERT_QCOUNT_LOW     = 1 << 13
	ALERT_QCOUNT_HIGH    = 1 << 12
	ALERT_VBAT_LOW       = 1 << 11
	ALERT_VBAT_HIGH      = 1 << 10
	ALERT_VIN_LOW        = 1 << 9
	ALERT_VIN_HIGH       = 1 << 8
	ALERT_VSYS_LOW       = 1 << 7
	ALERT_VSYS_HIGH      = 1 << 6
	ALERT_IIN_HIGH       = 1 << 5
	ALERT_IBAT_LOW       = 1 << 4
	ALERT_TEMP_HIGH      = 1 << 3
	ALERT_BSR_HIGH       = 1 << 2
	ALERT_NTC_RATIO_HIGH = 1 << 1
	ALERT_NTC_RATIO_LOW  = 1 << 0
)

// REG_CHARGER_STATE_STATUS
const (
	STATE_EQUALIZE         = 1 << 10
	STATE_ABSORB           = 1 << 9
	STATE_SUSPEND          = 1 << 8
	STATE_PRECHARGE        = 1 << 7
	STATE_CC_CV            = 1 << 6
	STATE_NTC_PAUSE        = 1 << 5
	STATE_TIMER_TERM       = 1 << 4
	STATE_C_OVER_X_TERM    = 1 << 3
	STATE_MAX_CHARGE_FAULT = 1 << 2
	STATE_BAT_MISSING      = 1 << 1
	STATE_BAT_SHORT        = 1 << 0
)

// REG_CHARGER_STATUS_STATUS
const (
	STATUS_UVCL_ACTIVE = 1 << 3
	STATUS_IIN_LIMIT   = 1 << 2
	STATUS_CC_ACTIVE   = 1 << 1
	STATUS_CV_ACTIVE   = 1 << 0
)

// REG_SYSTEM_STATUS
const (
	SYS_CHARGER_ENABLED    = 1 << 13
	SYS_MPPT_EN_PIN        = 1 << 11
	SYS_EQUALIZE_REQUESTED = 1 << 10
	SYS_DRVCC_GOOD         = 1 << 9
	SYS_CELL_COUNT_ERROR   = 1 << 8
	SYS_OK_TO_CHARGE       = 1 << 6
	SYS_NO_RT              = 1 << 5
	SYS_THERMAL_SHUTDOWN   = 1 << 4
	SYS_VIN_OVLO           = 1 << 3
	SYS_VIN_GT_VBAT        = 1 << 2
	SYS_INTVCC_GT_4V3      = 1 << 1
	SYS_INTVCC_GT_2V8      = 1 << 0
)

//
// ========== Measurement Scaling Factors (integer, fixed-point) ==========
//
// All values expressed in the smallest practical SI subunit to avoid floats.
// Voltage: microvolts per LSB (µV/LSB)
// Current: nanoamps per LSB (nA/LSB)
// Temperature: integer offset and scale (scaled by 10 for fixed-point arithmetic)
//

const (
	LSB_VBAT_LI_uV = 192264 // 192.264 µV/LSB (Lithium)
	LSB_VBAT_LA_uV = 128176 // 128.176 µV/LSB (Lead-acid)
	LSB_VIN_uV     = 1648   // 1.648 mV/LSB = 1648 µV/LSB
	LSB_VSYS_uV    = 1648   // 1.648 mV/LSB = 1648 µV/LSB
	LSB_CURR_nA    = 1465   // 1.46487 µA/LSB ≈ 1465 nA/LSB

	// Temperature calibration
	TEMP_OFFSET    = 12010 // ADC offset
	TEMP_SCALE_X10 = 456   // Scale factor 45.6, stored as 456 (x10)
)

// Additional scaling constants (avoid magic numbers in calculations)
const (
	MIN_CELLS = 1

	TEMP_SCALE_FACTOR = 100 // tenths of °C conversion factor

	BSR_DEN_LI = 500 // Lithium divisor
	BSR_DEN_LA = 750 // Lead-acid divisor

	NTC_RATIO_SCALE_PCT = 100   // scale NTC ratio to percent
	NTC_RATIO_DEN       = 21845 // datasheet-derived denominator

	QCOUNT_NUM_SCALE = 1_000_000_000 // numerator scaling factor
	QCOUNT_DEN_CONST = 29_999_988    // denominator constant from datasheet
)

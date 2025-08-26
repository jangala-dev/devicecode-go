// Package ltc4015 provides constants for register addresses and bitfields used
// in the operation of the LTC4015 battery charger controller.
package ltc4015

const (
	// 7-bit I2C address (1101_000b).
	AddressDefault = 0x68

	// --- CONFIG_BITS helpers (0x14) ---
	cfgSuspendCharger = 8
	cfgRunBSR         = 5
	cfgForceMeasSysOn = 4
	cfgMPPTEnableI2C  = 3
	cfgEnableQCount   = 2

	// --- Register sub-addresses (16-bit word registers) ---

	// Readouts / status
	regChargerState      = 0x34 // R
	regChargeStatus      = 0x35 // R/Clear
	regLimitAlerts       = 0x36 // R/Clear
	regChargerStateAlert = 0x37 // R/Clear
	regChargeStatAlerts  = 0x38 // R/Clear
	regSystemStatus      = 0x39 // R
	regVBAT              = 0x3A // R
	regVIN               = 0x3B // R
	regVSYS              = 0x3C // R
	regIBAT              = 0x3D // R
	regIIN               = 0x3E // R
	regDieTemp           = 0x3F // R
	regNTCRatio          = 0x40 // R
	regBSR               = 0x41 // R
	regChemCells         = 0x43 // R
	regMeasSysValid      = 0x4A // R, bit0

	// Config / control
	regConfigBits        = 0x14 // R/W (suspend, run_bsr, force_meas_sys_on, mppt_en_i2c, en_qcount)
	regIinLimitSetting   = 0x15 // R/W
	regVinUvclSetting    = 0x16 // R/W
	regEnLimitAlerts     = 0x0D // R/W
	regEnChargerStAlerts = 0x0E // R/W
	regEnChargeStAlerts  = 0x0F // R/W

	// Coulomb counter
	regQCountLoLimit  = 0x10 // R/W
	regQCountHiLimit  = 0x11 // R/W
	regQCountPrescale = 0x12 // R/W
	regQCount         = 0x13 // R/W
)

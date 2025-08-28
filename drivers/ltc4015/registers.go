// Package ltc4015 provides constants for register addresses and bitfields used
// in the operation of the LTC4015 battery charger controller.
package ltc4015

// -----------------------------------------------------------------------------
// Device I2C address
// -----------------------------------------------------------------------------

const (
	// 7-bit I2C address (1101_000b).
	AddressDefault = 0x68
	ARAAddress     = 0x19 // Alert Response Address (read-only)
)

// -----------------------------------------------------------------------------
// Register map (16-bit word registers)
// -----------------------------------------------------------------------------

// Alert limit threshold registers (write thresholds here; formats match telemetry).
// VBAT limits are per-cell.
const (
	regVBATLoAlertLimit     = 0x01 // VBAT format (per-cell)
	regVBATHiAlertLimit     = 0x02 // VBAT format (per-cell)
	regVINLoAlertLimit      = 0x03 // VIN format (1.648 mV/LSB)
	regVINHiAlertLimit      = 0x04 // VIN format (1.648 mV/LSB)
	regVSYSLoAlertLimit     = 0x05 // VSYS format (1.648 mV/LSB)
	regVSYSHiAlertLimit     = 0x06 // VSYS format (1.648 mV/LSB)
	regIINHiAlertLimit      = 0x07 // IIN format (1.46487 µV/RSNSI per LSB)
	regIBATLoAlertLimit     = 0x08 // IBAT format (1.46487 µV/RSNSB per LSB)
	regDieTempHiAlertLimit  = 0x09 // DIE_TEMP raw format
	regBSRHiAlertLimit      = 0x0A // BSR format (chemistry divisor)
	regNTCRatioHiAlertLimit = 0x0B // NTC_RATIO raw format (cold)
	regNTCRatioLoAlertLimit = 0x0C // NTC_RATIO raw format (hot)
)

// Alert enable registers.
const (
	regEnLimitAlerts     = 0x0D // R/W
	regEnChargerStAlerts = 0x0E // R/W
	regEnChargeStAlerts  = 0x0F // R/W
)

// Coulomb counter registers.
const (
	regQCountLoLimit  = 0x10 // R/W
	regQCountHiLimit  = 0x11 // R/W
	regQCountPrescale = 0x12 // R/W
	regQCount         = 0x13 // R/W
)

// Configuration / control registers.
const (
	regConfigBits      = 0x14 // R/W
	regIinLimitSetting = 0x15 // R/W
	regVinUvclSetting  = 0x16 // R/W
)

// --- Charger targets/timers and config
const (
	regIChargeTarget   = 0x1A // ICHARGE_TARGET (R/W)      [5-bit field]
	regVChargeSetting  = 0x1B // VCHARGE_SETTING (R/W)     [6-bit field, LA use]
	regCOverXThreshold = 0x1C // C/X_THRESHOLD (R/W)
	regMaxCVTime       = 0x1D // MAX_CV_TIME (R/W)         [s]
	regMaxChargeTime   = 0x1E // MAX_CHARGE_TIME (R/W)     [s]
	regChargerCfgBits  = 0x29 // CHARGER_CONFIG_BITS (R/W)
	regVAbsorbDelta    = 0x2A // VABSORB_DELTA (R/W)       [LA, per-cell]
	regMaxAbsorbTime   = 0x2B
	regVEqualizeDelta  = 0x2C // VEQUALIZE_DELTA (R/W)     [LA, per-cell]
	regEqualizeTime    = 0x2D // EQUALIZE_TIME (R/W)       [s]
)

// Readouts / status registers.
const (
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
	regMeasSysValid      = 0x4A // R (bit0 indicates valid)
)

// -----------------------------------------------------------------------------
// Bitfields (positions)
// -----------------------------------------------------------------------------

// CONFIG_BITS (0x14)
type ConfigBits uint16

const (
	CfgSuspendCharger ConfigBits = 1 << 8
	CfgRunBSR         ConfigBits = 1 << 5
	CfgForceMeasSysOn ConfigBits = 1 << 4
	CfgMPPTEnableI2C  ConfigBits = 1 << 3
	CfgEnableQCount   ConfigBits = 1 << 2
)

// CONFIG_BITS (0x14)
type ChargerCfgBits uint16

const (
	CfgEnCOverXTerm       ChargerCfgBits = 1 << 2
	CfgEnLeadAcidTempComp ChargerCfgBits = 1 << 1
	CfgEnJEITA            ChargerCfgBits = 1 << 0
)

// CHARGER_STATE (0x34, mutually exclusive where applicable)
type ChargerState uint16

const (
	StEqualizeCharge     ChargerState = 1 << 10
	StAbsorbCharge       ChargerState = 1 << 9
	StChargerSuspended   ChargerState = 1 << 8
	StPrecharge          ChargerState = 1 << 7
	StCcCvCharge         ChargerState = 1 << 6
	StNTCPause           ChargerState = 1 << 5
	StTimerTerm          ChargerState = 1 << 4
	StCOverXTerm         ChargerState = 1 << 3
	StMaxChargeTimeFault ChargerState = 1 << 2
	StBatMissingFault    ChargerState = 1 << 1
	StBatShortFault      ChargerState = 1 << 0
)

// SYSTEM_STATUS (0x39)
type SystemStatus uint16

const (
	SysChargerEnabled  SystemStatus = 1 << 13
	SysMpptEnPin       SystemStatus = 1 << 11
	SysEqualizeReq     SystemStatus = 1 << 10
	SysDrvccGood       SystemStatus = 1 << 9
	SysCellCountError  SystemStatus = 1 << 8
	SysOkToCharge      SystemStatus = 1 << 6
	SysNoRt            SystemStatus = 1 << 5
	SysThermalShutdown SystemStatus = 1 << 4
	SysVinOvlo         SystemStatus = 1 << 3
	SysVinGtVbat       SystemStatus = 1 << 2
	SysIntvccGt4p3V    SystemStatus = 1 << 1
	SysIntvccGt2p8V    SystemStatus = 1 << 0
)

// -----------------------------------------------------------------------------
// Typed masks using alias pattern (enables ↔ alerts share bit definitions)
// -----------------------------------------------------------------------------

// LIMIT_ALERTS / EN_LIMIT_ALERTS (0x36 / 0x0D)
type LimitBits uint16
type LimitEnable = LimitBits
type LimitAlerts = LimitBits

const (
	LaMeasSysValid LimitBits = 1 << 15
	// bit14 reserved
	LaQCountLo   LimitBits = 1 << 13
	LaQCountHi   LimitBits = 1 << 12
	LaVBATLo     LimitBits = 1 << 11
	LaVBATHi     LimitBits = 1 << 10
	LaVINLo      LimitBits = 1 << 9
	LaVINHi      LimitBits = 1 << 8
	LaVSYSLo     LimitBits = 1 << 7
	LaVSYSHi     LimitBits = 1 << 6
	LaIINHi      LimitBits = 1 << 5
	LaIBATLo     LimitBits = 1 << 4
	LaDieTempHi  LimitBits = 1 << 3
	LaBSRHi      LimitBits = 1 << 2
	LaNTCRatioHi LimitBits = 1 << 1 // cold
	LaNTCRatioLo LimitBits = 1 << 0 // hot
)

// CHARGER_STATE_ALERTS / EN_CHARGER_STATE_ALERTS (0x37 / 0x0E)
type ChargerStateBits uint16
type ChargerStateEnable = ChargerStateBits
type ChargerStateAlerts = ChargerStateBits

const (
	CsEqualizeCharge     ChargerStateBits = 1 << 10
	CsAbsorbCharge       ChargerStateBits = 1 << 9
	CsChargerSuspended   ChargerStateBits = 1 << 8
	CsPrecharge          ChargerStateBits = 1 << 7
	CsCCCVCharge         ChargerStateBits = 1 << 6
	CsNTCPause           ChargerStateBits = 1 << 5
	CsTimerTerm          ChargerStateBits = 1 << 4
	CsCOverXTerm         ChargerStateBits = 1 << 3
	CsMaxChargeTimeFault ChargerStateBits = 1 << 2
	CsBatMissingFault    ChargerStateBits = 1 << 1
	CsBatShortFault      ChargerStateBits = 1 << 0
)

// CHARGE_STATUS bits reused across status (0x35), enables (0x0F), and alerts (0x38).
type ChargeStatusBits uint16

// Aliases by register context
type ChargeStatus = ChargeStatusBits       // 0x35 (read-only status)
type ChargeStatusEnable = ChargeStatusBits // 0x0F (enable mask)
type ChargeStatusAlerts = ChargeStatusBits // 0x38 (R/Clear)

// Bit definitions (one set only)
const (
	CsVinUvclActive  ChargeStatusBits = 1 << 3
	CsIinLimitActive ChargeStatusBits = 1 << 2
	CsConstCurrent   ChargeStatusBits = 1 << 1
	CsConstVoltage   ChargeStatusBits = 1 << 0
)

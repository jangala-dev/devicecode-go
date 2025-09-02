// Package ltc4015 provides constants for register addresses and bitfields used
// in the operation of the LTC4015 battery charger controller.
package ltc4015

// -----------------------------------------------------------------------------
// Device I2C address
// -----------------------------------------------------------------------------

const (
	// 7-bit I2C address (1101_000b).
	AddressDefault = 0x68
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

// --- SMBus ARA
// SMBus Alert Response Address: datasheet quotes 0x19 (8-bit incl. R/W).
// TinyGo I2C expects a 7-bit address, which is 0x0C.
const ARAAddress = 0x0C

// --- Charger targets/timers and config (note: some parts are read-only in fixed-chem modes)
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

// Effective DAC read-backs (applied targets after algorithms/JEITA)
const (
	regIChargeDAC  = 0x44 // ICHARGE_DAC (R)
	regVChargeDAC  = 0x45 // VCHARGE_DAC (R)
	regIinLimitDAC = 0x46 // IIN_LIMIT_DAC (R)
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
	regIChargeBSR        = 0x48 // ICHARGE_BSR (R), IBAT used for BSR calc
	regMeasSysValid      = 0x4A // R (bit0 indicates valid)
)

// -----------------------------------------------------------------------------
// Bitfields (positions)
// -----------------------------------------------------------------------------

// CONFIG_BITS (0x14)
type ConfigBits uint16

const (
	SuspendCharger ConfigBits = 1 << 8
	RunBSR         ConfigBits = 1 << 5
	ForceMeasSysOn ConfigBits = 1 << 4
	MPPTEnableI2C  ConfigBits = 1 << 3
	EnableQCount   ConfigBits = 1 << 2
)

// CHARGER_CONFIG_BITS (0x29)
type ChargerCfgBits uint16

const (
	EnCOverXTerm       ChargerCfgBits = 1 << 2
	EnLeadAcidTempComp ChargerCfgBits = 1 << 1
	EnJEITA            ChargerCfgBits = 1 << 0
)

// SYSTEM_STATUS (0x39)
type SystemStatus uint16

const (
	ChargerEnabled  SystemStatus = 1 << 13
	MpptEnPin       SystemStatus = 1 << 11
	EqualizeReq     SystemStatus = 1 << 10
	DrvccGood       SystemStatus = 1 << 9
	CellCountError  SystemStatus = 1 << 8
	OkToCharge      SystemStatus = 1 << 6
	NoRt            SystemStatus = 1 << 5
	ThermalShutdown SystemStatus = 1 << 4
	VinOvlo         SystemStatus = 1 << 3
	VinGtVbat       SystemStatus = 1 << 2
	IntvccGt4p3V    SystemStatus = 1 << 1
	IntvccGt2p8V    SystemStatus = 1 << 0
)

// -----------------------------------------------------------------------------
// Typed masks using alias pattern (enables ↔ alerts share bit definitions)
// -----------------------------------------------------------------------------

// LIMIT_ALERTS / EN_LIMIT_ALERTS (0x36 / 0x0D)
type LimitBits uint16
type LimitEnable = LimitBits
type LimitAlerts = LimitBits

const (
	MeasSysValid LimitBits = 1 << 15
	// bit14 reserved
	QCountLo   LimitBits = 1 << 13
	QCountHi   LimitBits = 1 << 12
	VBATLo     LimitBits = 1 << 11
	VBATHi     LimitBits = 1 << 10
	VINLo      LimitBits = 1 << 9
	VINHi      LimitBits = 1 << 8
	VSYSLo     LimitBits = 1 << 7
	VSYSHi     LimitBits = 1 << 6
	IINHi      LimitBits = 1 << 5
	IBATLo     LimitBits = 1 << 4
	DieTempHi  LimitBits = 1 << 3
	BSRHi      LimitBits = 1 << 2
	NTCRatioHi LimitBits = 1 << 1 // cold
	NTCRatioLo LimitBits = 1 << 0 // hot
)

// CHARGER_STATE_ALERTS / EN_CHARGER_STATE_ALERTS (0x37 / 0x0E)
type ChargerStateBits uint16
type ChargerState = ChargerStateBits
type ChargerStateEnable = ChargerStateBits
type ChargerStateAlerts = ChargerStateBits

const (
	EqualizeCharge     ChargerStateBits = 1 << 10
	AbsorbCharge       ChargerStateBits = 1 << 9
	ChargerSuspended   ChargerStateBits = 1 << 8
	Precharge          ChargerStateBits = 1 << 7
	CCCVCharge         ChargerStateBits = 1 << 6
	NTCPause           ChargerStateBits = 1 << 5
	TimerTerm          ChargerStateBits = 1 << 4
	COverXTerm         ChargerStateBits = 1 << 3
	MaxChargeTimeFault ChargerStateBits = 1 << 2
	BatMissingFault    ChargerStateBits = 1 << 1
	BatShortFault      ChargerStateBits = 1 << 0
)

// CHARGE_STATUS bits reused across status (0x35), enables (0x0F), and alerts (0x38).
type ChargeStatusBits uint16

// Aliases by register context
type ChargeStatus = ChargeStatusBits       // 0x35 (read-only status)
type ChargeStatusEnable = ChargeStatusBits // 0x0F (enable mask)
type ChargeStatusAlerts = ChargeStatusBits // 0x38 (R/Clear)

// Bit definitions (one set only)
const (
	VinUvclActive  ChargeStatusBits = 1 << 3
	IinLimitActive ChargeStatusBits = 1 << 2
	ConstCurrent   ChargeStatusBits = 1 << 1
	ConstVoltage   ChargeStatusBits = 1 << 0
)

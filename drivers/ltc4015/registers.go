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

// --- Alert limit threshold registers (write your thresholds here; formats
// match the corresponding telemetry registers). VBAT limits are per-cell.
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

// --- Alert enable registers (set bitmasks below with Enable* helpers).
const (
	regEnLimitAlerts     = 0x0D // R/W
	regEnChargerStAlerts = 0x0E // R/W
	regEnChargeStAlerts  = 0x0F // R/W
)

// --- Coulomb counter registers.
const (
	regQCountLoLimit  = 0x10 // R/W
	regQCountHiLimit  = 0x11 // R/W
	regQCountPrescale = 0x12 // R/W
	regQCount         = 0x13 // R/W
)

// --- Configuration / control registers.
const (
	regConfigBits      = 0x14 // R/W
	regIinLimitSetting = 0x15 // R/W
	regVinUvclSetting  = 0x16 // R/W
)

// --- Readouts / status registers.
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
const (
	cfgSuspendCharger = 8
	cfgRunBSR         = 5
	cfgForceMeasSysOn = 4
	cfgMPPTEnableI2C  = 3
	cfgEnableQCount   = 2
)

// CHARGER_STATE (0x34, mutually exclusive where applicable)
const (
	stEqualizeCharge     = 10
	stAbsorbCharge       = 9
	stChargerSuspended   = 8
	stPrecharge          = 7
	stCcCvCharge         = 6
	stNtcPause           = 5
	stTimerTerm          = 4
	stCOverXTerm         = 3
	stMaxChargeTimeFault = 2
	stBatMissingFault    = 1 // battery missing
	stBatShortFault      = 0 // shorted battery
)

// CHARGE_STATUS (0x35, mutually exclusive while charging)
const (
	csVinUvclActive  = 3
	csIinLimitActive = 2
	csConstCurrent   = 1
	csConstVoltage   = 0
)

// SYSTEM_STATUS (0x39)
const (
	sysChargerEnabled  = 13
	sysMpptEnPin       = 11
	sysEqualizeReq     = 10
	sysDrvccGood       = 9
	sysCellCountError  = 8
	sysOkToCharge      = 6
	sysNoRt            = 5
	sysThermalShutdown = 4
	sysVinOvlo         = 3
	sysVinGtVbat       = 2 // VIN ≥ ~200 mV above VBAT
	sysIntvccGt4p3V    = 1
	sysIntvccGt2p8V    = 0
)

// LIMIT_ALERTS (0x36, read/clear)
const (
	laMeasSysValidAlert = 15
	// bit14 reserved
	laQcountLoAlert   = 13
	laQcountHiAlert   = 12
	laVbatLoAlert     = 11
	laVbatHiAlert     = 10
	laVinLoAlert      = 9
	laVinHiAlert      = 8
	laVsysLoAlert     = 7
	laVsysHiAlert     = 6
	laIinHiAlert      = 5
	laIbatLoAlert     = 4
	laDieTempHiAlert  = 3
	laBsrHiAlert      = 2
	laNtcRatioHiAlert = 1 // cold
	laNtcRatioLoAlert = 0 // hot
)

// -----------------------------------------------------------------------------
// Typed enable masks (for EN_* registers)
// -----------------------------------------------------------------------------

// EN_LIMIT_ALERTS (0x0D)
type LimitEnable uint16

const (
	EnMeasSysValid LimitEnable = 1 << 15
	// bit14 reserved
	EnQCountLo   LimitEnable = 1 << 13
	EnQCountHi   LimitEnable = 1 << 12
	EnVBATLo     LimitEnable = 1 << 11
	EnVBATHi     LimitEnable = 1 << 10
	EnVINLo      LimitEnable = 1 << 9
	EnVINHi      LimitEnable = 1 << 8
	EnVSYSLo     LimitEnable = 1 << 7
	EnVSYSHi     LimitEnable = 1 << 6
	EnIINHi      LimitEnable = 1 << 5
	EnIBATLo     LimitEnable = 1 << 4
	EnDieTempHi  LimitEnable = 1 << 3
	EnBSRHi      LimitEnable = 1 << 2
	EnNTCRatioHi LimitEnable = 1 << 1 // cold
	EnNTCRatioLo LimitEnable = 1 << 0 // hot
)

// EN_CHARGER_STATE_ALERTS (0x0E)
type ChargerStateEnable uint16

const (
	EnEqualizeCharge     ChargerStateEnable = 1 << 10
	EnAbsorbCharge       ChargerStateEnable = 1 << 9
	EnChargerSuspended   ChargerStateEnable = 1 << 8
	EnPrecharge          ChargerStateEnable = 1 << 7
	EnCCCVCharge         ChargerStateEnable = 1 << 6
	EnNTCPause           ChargerStateEnable = 1 << 5
	EnTimerTerm          ChargerStateEnable = 1 << 4
	EnCOverXTerm         ChargerStateEnable = 1 << 3
	EnMaxChargeTimeFault ChargerStateEnable = 1 << 2
	EnBatMissingFault    ChargerStateEnable = 1 << 1
	EnBatShortFault      ChargerStateEnable = 1 << 0
)

// EN_CHARGE_STATUS_ALERTS (0x0F)
type ChargeStatusEnable uint16

const (
	EnVinUVCLActive   ChargeStatusEnable = 1 << 3
	EnIinLimitActive  ChargeStatusEnable = 1 << 2
	EnConstantCurrent ChargeStatusEnable = 1 << 1
	EnConstantVoltage ChargeStatusEnable = 1 << 0
)

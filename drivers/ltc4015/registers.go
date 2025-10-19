package ltc4015

// Device I2C address (7-bit).
const AddressDefault = 0x68

// SMBus Alert Response Address (7-bit form for TinyGo).
const ARAAddress = 0x0C

// -----------------------------------------------------------------------------
// Register map (16-bit word registers)
// -----------------------------------------------------------------------------

// Alert limit threshold registers (formats match telemetry). VBAT limits are per-cell.
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

// JEITA thresholds and targets (lithium).
const (
	regJEITAT1 = 0x1F // R/W (NTC_RATIO)
	regJEITAT2 = 0x20 // R/W
	regJEITAT3 = 0x21 // R/W
	regJEITAT4 = 0x22 // R/W
	regJEITAT5 = 0x23 // R/W
	regJEITAT6 = 0x24 // R/W

	regJEITAVchg_2_4 = 0x26 // VCHARGE codes: R/W (regions 2..4 packed)
	regJEITAVchg_5_6 = 0x25 // VCHARGE codes: R/W (regions 5..6 packed)
	regJEITAIchg_2_4 = 0x28 // ICHARGE codes: R/W (regions 2..4 packed)
	regJEITAIchg_5_6 = 0x27 // ICHARGE codes: R/W (regions 5..6 packed)
)

// Targets/timers (note: some parts are read-only in fixed-chem modes).
const (
	regIChargeTarget   = 0x1A // ICHARGE_TARGET (R/W)      [5-bit field]
	regVChargeSetting  = 0x1B // VCHARGE_SETTING (R/W)     [6-bit LA field]
	regCOverXThreshold = 0x1C // C/X_THRESHOLD (R/W)
	regMaxCVTime       = 0x1D // MAX_CV_TIME (R/W)         [s]
	regMaxChargeTime   = 0x1E // MAX_CHARGE_TIME (R/W)     [s]
	regChargerCfgBits  = 0x29 // CHARGER_CONFIG_BITS (R/W)
	regVAbsorbDelta    = 0x2A // VABSORB_DELTA (R/W)       [LA & LiFePO4, per-cell]
	regMaxAbsorbTime   = 0x2B // MAX_ABSORB_TIME (R/W)     [s]
	regVEqualizeDelta  = 0x2C // VEQUALIZE_DELTA (R/W)     [LA, per-cell]
	regEqualizeTime    = 0x2D // EQUALIZE_TIME (R/W)       [s]
	regLiFePO4RchgTh   = 0x2E // LiFePO4_RECHARGE_THRESHOLD (R/W) [per-cell]
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
	regIChargeBSR        = 0x48 // ICHARGE_BSR (R)
	regMeasSysValid      = 0x4A // R (bit0 indicates valid)

	// Timer read-backs (exposed as convenience helpers).
	regCVTimer       = 0x31 // lithium CV timer (R)
	regAbsorbTimer   = 0x32 // LiFePO4 / Lead-acid absorb timer (R)
	regEqualizeTimer = 0x33 // Lead-acid equalise timer (R)
)

// -----------------------------------------------------------------------------
// Bitfields
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

// CHARGE_STATUS (0x35 / 0x0F / 0x38) shared bit definitions
type ChargeStatusBits uint16
type ChargeStatus = ChargeStatusBits       // 0x35 (R)
type ChargeStatusEnable = ChargeStatusBits // 0x0F (enable mask)
type ChargeStatusAlerts = ChargeStatusBits // 0x38 (R/Clear)

const (
	VinUvclActive  ChargeStatusBits = 1 << 3
	IinLimitActive ChargeStatusBits = 1 << 2
	ConstCurrent   ChargeStatusBits = 1 << 1
	ConstVoltage   ChargeStatusBits = 1 << 0
)

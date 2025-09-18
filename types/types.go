package types

import "time"

// ---------------- Common enums ----------------

type LinkState uint8

const (
	LinkDown LinkState = iota
	LinkUp
	LinkDegraded
)

type Edge uint8

const (
	EdgeNone Edge = iota
	EdgeRising
	EdgeFalling
	EdgeBoth
)

type UARTDir uint8

const (
	UARTRx UARTDir = iota
	UARTTx
)

type Chemistry uint8

const (
	ChemUnknown Chemistry = iota
	ChemLithium
	ChemLeadAcid
)

type ChargerPhase uint8

const (
	PhaseIdle ChargerPhase = iota
	PhasePrecharge
	PhaseCC
	PhaseCV
	PhaseAbsorb
	PhaseEqualize
	PhaseSuspended
	PhaseFault
)

// ---------------- HAL service state ----------------

type HALState struct {
	Level  string
	Status string
	Error  string
	TS     time.Time
}

// ---------------- Capability “info” (retained) ----------------

type GPIOInfo struct {
	SchemaVersion uint8
	Driver        string
	Pin           int
	Mode          string // "input" | "output"
	Pull          string // "up" | "down" | "none" (only for input)
	Invert        bool
}

type UARTInfo struct {
	SchemaVersion uint8
	Driver        string
}

type TemperatureInfo struct {
	SchemaVersion uint8
	Driver        string
	Unit          string
	Precision     float32
}

type HumidityInfo struct {
	SchemaVersion uint8
	Driver        string
	Unit          string
	Precision     float32
}

type PowerUnits struct {
	VBatPerCell_mV  string
	VBatPack_mV     string
	Vin_mV          string
	Vsys_mV         string
	IBat_mA         string
	IIn_mA          string
	Die_mC          string
	BSR_uohmPerCell string
	IChargeDAC_mA   string
	IInLimitDAC_mA  string
	IChargeBSR_mA   string
}

type PowerInfo struct {
	SchemaVersion uint8
	Driver        string
	Cells         uint8
	Chemistry     Chemistry
	Units         PowerUnits
}

type ChargerBitfields struct {
	SystemStatus map[int]string
	ChargerState map[int]string
	ChargeStatus map[int]string
}

type ChargerInfo struct {
	SchemaVersion   uint8
	Model           string
	Chemistry       Chemistry
	Cells           uint8
	TargetsWritable bool
	VendorBitfields ChargerBitfields
}

type AlertsInfo struct {
	SchemaVersion uint8
	Groups        []string
}

// ---------------- Values / state / events ----------------

type CapabilityState struct {
	Link  LinkState
	TS    time.Time
	Error string // optional, empty when OK

}

type GPIOState struct {
	Link  LinkState
	Level uint8
	TS    time.Time
}

type GPIOEvent struct {
	Edge  Edge
	Level uint8
	TS    time.Time
}

type UARTEvent struct {
	Dir  UARTDir
	Data []byte
	N    int
	TS   time.Time
}

type IntValue struct {
	Value int32
	TS    time.Time
}

type TemperatureValue struct {
	DeciC int32
	TS    time.Time
}

type HumidityValue struct {
	DeciPercent int32
	TS          time.Time
}

type PowerValue struct {
	TS              time.Time
	VBatPerCell_mV  *int32
	VBatPack_mV     *int32
	Vin_mV          *int32
	Vsys_mV         *int32
	IBat_mA         *int32
	IIn_mA          *int32
	Die_mC          *int32
	BSR_uohmPerCell *uint32
	IChargeDAC_mA   *int32
	IInLimitDAC_mA  *int32
	IChargeBSR_mA   *int32
}

type ChargerFaults struct {
	BatMissing      bool
	BatShort        bool
	ThermalShutdown bool
}

type ChargerInputLimited struct {
	VinUvcl  bool
	IInLimit bool
}

type ChargerRaw struct {
	SystemStatus uint16
	ChargerState uint16
	ChargeStatus uint16
}

type ChargerValue struct {
	Phase        ChargerPhase
	InputLimited ChargerInputLimited
	OKToCharge   bool
	Faults       ChargerFaults
	Raw          ChargerRaw
	TS           time.Time
}

type AlertsEvent struct {
	Limit     uint16
	ChgState  uint16
	ChgStatus uint16
	TS        time.Time
}

// ---------------- Control plane ----------------

type ReadNow struct{}
type ReadNowAck struct{ OK bool }

type SetRate struct{ PeriodMS int }
type SetRateAck struct {
	OK       bool
	PeriodMS int
}

type GPIOGet struct{}
type GPIOGetReply struct{ Level uint8 }
type GPIOSet struct{ Level bool }

type UARTWrite struct{ Data []byte }
type UARTWriteReply struct {
	OK bool
	N  int
}
type UARTSetBaud struct{ Baud uint32 }
type Parity uint8

const (
	ParityNone Parity = iota
	ParityEven
	ParityOdd
)

type UARTSetFormat struct {
	DataBits uint8
	StopBits uint8
	Parity   Parity
}

// LTC4015 typed controls
type LTC4015SetInputCurrentLimit struct{ MA int } // set_input_current_limit
type LTC4015SetChargeCurrent struct{ MA int }     // set_charge_current
type LTC4015SetVinUVCL struct{ MV int }           // set_vin_uvcl

type LTC4015ReadAlerts struct{}
type LTC4015ReadAlertsReply struct {
	Limit     uint16
	ChgState  uint16
	ChgStatus uint16
}
type LTC4015ApplyProfile struct {
	Chemistry         Chemistry
	Cells             *int
	VCharge_mVPerCell *int32
	VAbsorbDelta_mV   *int32
	VEqualizeDelta_mV *int32
	MaxAbsorbTime_s   *uint16
	EqualizeTime_s    *uint16
	EnableTempComp    *bool
}

type LTC4015ApplyProfileReply struct {
	OK  bool
	Raw ChargerRaw
}

// ---------------- Generic error reply ----------------

type ErrorReply struct {
	OK    bool
	Error string
}

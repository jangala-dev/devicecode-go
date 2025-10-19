package ltc4015

import (
	"errors"

	"tinygo.org/x/drivers"
)

// Public chemistry families.
type Chemistry uint8

const (
	ChemUnknown  Chemistry = iota
	ChemLithium            // VBAT LSB: 192.264 µV/cell
	ChemLeadAcid           // VBAT LSB: 128.176 µV/cell
)

// Enumerates strapped/device variants (pins + die option).
type ChemVariant uint8

const (
	ChemVarUnknown ChemVariant = iota
	ChemVarLiIonProg
	ChemVarLiIonFix42
	ChemVarLiIonFix41
	ChemVarLiIonFix40
	ChemVarLiFePO4Prog
	ChemVarLiFePO4FixFast
	ChemVarLiFePO4Fix36
	ChemVarLeadAcidFix
	ChemVarLeadAcidProg
)

func (v ChemVariant) IsLithium() bool {
	switch v {
	case ChemVarLiIonProg, ChemVarLiIonFix42, ChemVarLiIonFix41, ChemVarLiIonFix40,
		ChemVarLiFePO4Prog, ChemVarLiFePO4FixFast, ChemVarLiFePO4Fix36:
		return true
	default:
		return false
	}
}

func (v ChemVariant) IsLiFePO4() bool {
	switch v {
	case ChemVarLiFePO4Prog, ChemVarLiFePO4FixFast, ChemVarLiFePO4Fix36:
		return true
	default:
		return false
	}
}

func (v ChemVariant) IsProgrammable() bool {
	switch v {
	case ChemVarLiIonProg, ChemVarLiFePO4Prog, ChemVarLeadAcidProg:
		return true
	default:
		return false
	}
}

var (
	ErrTargetsReadOnly  = errors.New("targets/timers are read-only in fixed-chem mode")
	ErrChemistryUnknown = errors.New("unable to determine chemistry")
)

// Driver configuration. Integer-only.
type Config struct {
	Address         uint16
	RSNSB_uOhm      uint32 // battery path sense resistor in µΩ
	RSNSI_uOhm      uint32 // input path sense resistor in µΩ
	Cells           uint8  // optional; read from pins if 0
	Chem            Chemistry
	QCountPrescale  uint16 // if 0, leave hardware default
	TargetsWritable bool   // set false if using a fixed-chem variant
}

// DefaultConfig provides minimal defaults; caller must set sense resistors.
func DefaultConfig() Config {
	return Config{
		Address:         AddressDefault,
		Chem:            ChemLithium,
		TargetsWritable: true,
	}
}

// Validate basic required fields used by many APIs.
func (c Config) Validate() error {
	if c.Address == 0 {
		return errors.New("Address must be non-zero (use AddressDefault)")
	}
	if c.RSNSB_uOhm == 0 {
		return errors.New("RSNSB_uOhm must be set (battery path sense)")
	}
	if c.RSNSI_uOhm == 0 {
		return errors.New("RSNSI_uOhm must be set (input path sense)")
	}
	return nil
}

// Device represents an LTC4015 instance on an I²C bus.
type Device struct {
	i2c   drivers.I2C
	addr  uint16
	cells uint8
	chem  Chemistry

	variant ChemVariant

	rsnsB_uOhm      uint32
	rsnsI_uOhm      uint32
	targetsWritable bool

	// Fixed buffers to avoid per-call heap allocations.
	w [3]byte
	r [2]byte
}

// New constructs a Device with supplied config.
func New(i2c drivers.I2C, cfg Config) *Device {
	addr := cfg.Address
	if addr == 0 {
		addr = AddressDefault
	}
	chem := cfg.Chem
	if chem == ChemUnknown {
		chem = ChemLithium
	}
	return &Device{
		i2c:             i2c,
		addr:            addr,
		cells:           cfg.Cells,
		chem:            chem,
		rsnsB_uOhm:      cfg.RSNSB_uOhm,
		rsnsI_uOhm:      cfg.RSNSI_uOhm,
		targetsWritable: cfg.TargetsWritable,
	}
}

// Configure applies runtime changes. Chemistry is not changed here.
func (d *Device) Configure(cfg Config) error {
	// Cells from caller or pins.
	if cfg.Cells != 0 {
		d.cells = cfg.Cells
	} else {
		if v, err := d.readWord(regChemCells); err == nil {
			d.cells = uint8(v & 0x000F)
		}
	}
	if cfg.RSNSB_uOhm != 0 {
		d.rsnsB_uOhm = cfg.RSNSB_uOhm
	}
	if cfg.RSNSI_uOhm != 0 {
		d.rsnsI_uOhm = cfg.RSNSI_uOhm
	}
	if cfg.QCountPrescale != 0 {
		if err := d.writeWord(regQCountPrescale, cfg.QCountPrescale); err != nil {
			return err
		}
	}
	if !cfg.TargetsWritable {
		d.targetsWritable = false
	}
	return nil
}

// NewAuto constructs a Device and detects variant/chemistry once.
// Also infers target/timer writability.
func NewAuto(i2c drivers.I2C, cfg Config) (*Device, error) {
	d := New(i2c, cfg)

	// Cells from pins if not supplied.
	if d.cells == 0 {
		if v, err := d.readWord(regChemCells); err == nil {
			d.cells = uint8(v & 0x000F)
		}
	}

	vt, err := d.DetectVariant()
	if err != nil {
		return nil, err
	}
	d.variant = vt
	if vt == ChemVarUnknown {
		return nil, ErrChemistryUnknown
	}

	if vt.IsLithium() {
		d.chem = ChemLithium
	} else {
		d.chem = ChemLeadAcid
	}

	// Infer; caller may only force false.
	d.targetsWritable = vt.IsProgrammable()
	if !cfg.TargetsWritable {
		d.targetsWritable = false
	}
	return d, nil
}

// Introspection.
func (d *Device) Chem() Chemistry       { return d.chem }
func (d *Device) Cells() uint8          { return d.cells }
func (d *Device) Variant() ChemVariant  { return d.variant }
func (d *Device) TargetsWritable() bool { return d.targetsWritable }

// Detection.
func (d *Device) DetectCells() (uint8, error) {
	v, err := d.readWord(regChemCells)
	if err != nil {
		return 0, err
	}
	return uint8(v & 0x000F), nil
}

func (d *Device) DetectChemistry() (Chemistry, error) {
	v, err := d.readWord(regChemCells)
	if err != nil {
		return ChemUnknown, err
	}
	chemCode := (v >> 8) & 0x000F
	switch chemCode {
	case 0x7, 0x8:
		return ChemLeadAcid, nil
	case 0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6:
		return ChemLithium, nil
	default:
		return ChemUnknown, nil
	}
}

func (d *Device) DetectVariant() (ChemVariant, error) {
	v, err := d.readWord(regChemCells)
	if err != nil {
		return ChemVarUnknown, err
	}
	code := byte((v >> 8) & 0x0F)
	switch code {
	case 0x0:
		return ChemVarLiIonProg, nil
	case 0x1:
		return ChemVarLiIonFix42, nil
	case 0x2:
		return ChemVarLiIonFix41, nil
	case 0x3:
		return ChemVarLiIonFix40, nil
	case 0x4:
		return ChemVarLiFePO4Prog, nil
	case 0x5:
		return ChemVarLiFePO4FixFast, nil
	case 0x6:
		return ChemVarLiFePO4Fix36, nil
	case 0x7:
		return ChemVarLeadAcidFix, nil
	case 0x8:
		return ChemVarLeadAcidProg, nil
	default:
		return ChemVarUnknown, nil
	}
}

// Config register helpers (typed, minimal API).

func (b ConfigBits) Has(flag ConfigBits) bool { return b&flag != 0 }

func (d *Device) ReadConfig() (ConfigBits, error) {
	v, err := d.readWord(regConfigBits)
	return ConfigBits(v), err
}
func (d *Device) WriteConfig(v ConfigBits) error { return d.writeWord(regConfigBits, uint16(v)) }
func (d *Device) SetConfigBits(mask ConfigBits) error {
	return d.modifyBitmaskRegister(regConfigBits, uint16(mask), 0)
}
func (d *Device) ClearConfigBits(mask ConfigBits) error {
	return d.modifyBitmaskRegister(regConfigBits, 0, uint16(mask))
}
func (d *Device) UpdateConfig(set, clear ConfigBits) error {
	return d.modifyBitmaskRegister(regConfigBits, uint16(set), uint16(clear))
}

// Charger config bits.

func (b ChargerCfgBits) Has(flag ChargerCfgBits) bool { return b&flag != 0 }

func (d *Device) ReadChargerConfig() (ChargerCfgBits, error) {
	v, err := d.readWord(regChargerCfgBits)
	return ChargerCfgBits(v), err
}
func (d *Device) WriteChargerConfig(v ChargerCfgBits) error {
	return d.writeWord(regChargerCfgBits, uint16(v))
}
func (d *Device) SetChargerConfigBits(mask ChargerCfgBits) error {
	return d.modifyBitmaskRegister(regChargerCfgBits, uint16(mask), 0)
}
func (d *Device) ClearChargerConfigBits(mask ChargerCfgBits) error {
	return d.modifyBitmaskRegister(regChargerCfgBits, 0, uint16(mask))
}
func (d *Device) UpdateChargerConfig(set, clear ChargerCfgBits) error {
	return d.modifyBitmaskRegister(regChargerCfgBits, uint16(set), uint16(clear))
}

// Bitmask helpers.
func (b LimitBits) Has(flag LimitBits) bool               { return b&flag != 0 }
func (b ChargerStateBits) Has(flag ChargerStateBits) bool { return b&flag != 0 }
func (b ChargeStatusBits) Has(flag ChargeStatusBits) bool { return b&flag != 0 }
func (b SystemStatus) Has(flag SystemStatus) bool         { return b&flag != 0 }

// Guard for programmable targets/timers.
func (d *Device) ensureTargetsWritable() error {
	if !d.targetsWritable {
		return ErrTargetsReadOnly
	}
	return nil
}

// Generic read-modify-write for 16-bit registers with bitmasks.
func (d *Device) modifyBitmaskRegister(regAddr byte, set, clear uint16) error {
	current, err := d.readWord(regAddr)
	if err != nil {
		return err
	}
	newVal := (current | set) &^ clear
	return d.writeWord(regAddr, newVal)
}

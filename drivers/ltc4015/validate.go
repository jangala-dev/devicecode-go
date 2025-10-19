package ltc4015

import "errors"

var (
	// Sentinel errors (TinyGo-safe; no fmt)
	ErrUnknownChemParam     = errors.New("unknown expected chemistry")
	ErrVariantNotLeadAcid   = errors.New("not lead-acid variant")
	ErrVariantNotLiFePO4    = errors.New("not LiFePO4 variant")
	ErrVariantNotLithiumIon = errors.New("not lithium-ion variant")
	ErrCellsMismatch        = errors.New("cells mismatch")
	ErrLeadAcidCellsInvalid = errors.New("lead-acid cells must be 3/6/12")

	// Sense resistor unset.
	ErrRSNSBUnset = errors.New("RSNSB_uOhm must be set for battery current operations")
	ErrRSNSIUnset = errors.New("RSNSI_uOhm must be set for input current operations")
)

// ExpectChem describes the expected chemistry family for validation.
type ExpectChem uint8

const (
	// ExpectLithiumIon means lithium-ion family excluding LiFePO4.
	ExpectLithiumIon ExpectChem = iota + 1
	// ExpectLiFePO4 means LiFePO4 family.
	ExpectLiFePO4
	// ExpectLeadAcid means lead-acid family.
	ExpectLeadAcid
)

// ValidateAgainst checks detected variant and pins-based cell strap against expectations.
func (d *Device) ValidateAgainst(expChem ExpectChem, expCells uint8) error {
	vt := d.Variant()
	switch expChem {
	case ExpectLeadAcid:
		if !(vt == ChemVarLeadAcidProg || vt == ChemVarLeadAcidFix) {
			return ErrVariantNotLeadAcid
		}
	case ExpectLiFePO4:
		if !vt.IsLiFePO4() {
			return ErrVariantNotLiFePO4
		}
	case ExpectLithiumIon:
		if !(vt.IsLithium() && !vt.IsLiFePO4()) {
			return ErrVariantNotLithiumIon
		}
	default:
		return ErrUnknownChemParam
	}
	if got := d.Cells(); got != 0 && expCells != 0 && got != expCells {
		return ErrCellsMismatch
	}
	if expChem == ExpectLeadAcid {
		if c := d.Cells(); c != 0 {
			switch c {
			case 3, 6, 12:
			default:
				return ErrLeadAcidCellsInvalid
			}
		}
	}
	return nil
}

// Package ltc4015 — chemistry-specific views over the common Device.
//
// Concurrency: methods are not safe for concurrent use from multiple goroutines.
// Serialise calls externally if needed.
package ltc4015

import "tinygo.org/x/drivers"

// LeadAcid exposes lead-acid–only operations.
type LeadAcid struct{ d *Device }

// Lithium exposes lithium-specific helpers (extend as needed).
type Lithium struct{ d *Device }

// LeadAcid returns a lead-acid view if the configured chemistry is lead-acid.
func (d *Device) LeadAcid() (LeadAcid, bool) {
	return LeadAcid{d: d}, d.chem == ChemLeadAcid
}

// Lithium returns a lithium view if the configured chemistry is lithium.
func (d *Device) Lithium() (Lithium, bool) {
	return Lithium{d: d}, d.chem == ChemLithium
}

// NewAuto constructs a Device and, if Chem==ChemUnknown, detects chemistry once.
func NewAuto(i2c drivers.I2C, cfg Config) (*Device, error) {
	d := New(i2c, cfg)
	if d.chem == ChemUnknown {
		c, err := d.DetectChemistry()
		if err != nil {
			return nil, err
		}
		if c == ChemUnknown {
			return nil, ErrChemistryUnknown
		}
		d.chem = c
	}
	return d, nil
}

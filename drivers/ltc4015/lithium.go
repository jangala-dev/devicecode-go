package ltc4015

// Optional lithium-focused helpers.

// EnableJEITA toggles the JEITA control bit in CHARGER_CONFIG_BITS.
// Note: with JEITA enabled, effective targets derive from the JEITA tables.
func (li Lithium) EnableJEITA(on bool) error {
	if on {
		return li.d.SetChargerConfigBits(CfgEnJEITA)
	}
	return li.d.ClearChargerConfigBits(CfgEnJEITA)
}

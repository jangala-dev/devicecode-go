package ltc4015

// Lead-acid–only methods live on LeadAcid; no chemistry guard is needed.

// VCHARGE_SETTING per-cell. See datasheet for optional cap with tempComp.
func (la LeadAcid) SetVChargeSetting_mVPerCell(mV int32, tempComp bool) error {
	if err := la.d.ensureTargetsWritable(); err != nil {
		return err
	}
	code := laCountsFrommV(mV, 2000)
	if tempComp && code > 35 {
		code = 35
	}
	code = clampRange(code, 0, 63)
	return la.d.writeWord(regVChargeSetting, uint16(code))
}

// Absorb delta per-cell (same scaling, offset 0).
func (la LeadAcid) SetVAbsorbDelta_mVPerCell(delta int32) error {
	if err := la.d.ensureTargetsWritable(); err != nil {
		return err
	}
	code := clampRange(laCountsFrommV(delta, 0), 0, 63)
	return la.d.writeWord(regVAbsorbDelta, uint16(code))
}

// Equalise delta per-cell (same scaling, offset 0).
func (la LeadAcid) SetVEqualizeDelta_mVPerCell(delta int32) error {
	if err := la.d.ensureTargetsWritable(); err != nil {
		return err
	}
	code := clampRange(laCountsFrommV(delta, 0), 0, 63)
	return la.d.writeWord(regVEqualizeDelta, uint16(code))
}

// MAX_CV_TIME (s).
func (la LeadAcid) SetMaxAbsorbTime_s(sec uint16) error {
	if err := la.d.ensureTargetsWritable(); err != nil {
		return err
	}
	return la.d.writeWord(regMaxAbsorbTime, sec)
}

// EQUALIZE_TIME (s).
func (la LeadAcid) SetEqualizeTime_s(sec uint16) error {
	if err := la.d.ensureTargetsWritable(); err != nil {
		return err
	}
	return la.d.writeWord(regEqualizeTime, sec)
}

// Toggle lead-acid temperature compensation bit in CHARGER_CONFIG_BITS.
func (la LeadAcid) EnableLeadAcidTempComp(on bool) error {
	if on {
		return la.d.SetChargerConfigBits(EnLeadAcidTempComp)
	}
	return la.d.ClearChargerConfigBits(EnLeadAcidTempComp)
}

// Convenience: read back applied VCHARGE per-cell from VCHARGE_DAC (LA mapping).
func (la LeadAcid) VChargeDAC_mVPerCell() (int32, error) {
	code, err := la.d.readWord(regVChargeDAC)
	if err != nil {
		return 0, err
	}
	// mV ≈ 2000 + code*(1000/105)
	mV := int64(2000) + (int64(code)*1000)/105
	return int32(mV), nil
}

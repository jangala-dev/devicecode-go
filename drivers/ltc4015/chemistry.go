package ltc4015

// Chemistry-specific views over Device.

type LeadAcid struct{ d *Device }
type Lithium struct{ d *Device }
type LiFePO4 struct{ d *Device }

// Views

func (d *Device) LeadAcid() (LeadAcid, bool) { return LeadAcid{d: d}, d.chem == ChemLeadAcid }
func (d *Device) Lithium() (Lithium, bool)   { return Lithium{d: d}, d.chem == ChemLithium }
func (d *Device) LiFePO4() (LiFePO4, bool)   { return LiFePO4{d: d}, d.variant.IsLiFePO4() }

// ----- Lithium (shared: Li-ion & LiFePO4) -----

func (li Lithium) EnableJEITA(on bool) error {
	if on {
		return li.d.SetChargerConfigBits(EnJEITA)
	}
	return li.d.ClearChargerConfigBits(EnJEITA)
}

func (li Lithium) SetMaxCVTime_s(sec uint16) error {
	if err := li.d.ensureTargetsWritable(); err != nil {
		return err
	}
	return li.d.writeWord(regMaxCVTime, sec)
}

func (li Lithium) SetMaxChargeTime_s(sec uint16) error {
	if err := li.d.ensureTargetsWritable(); err != nil {
		return err
	}
	return li.d.writeWord(regMaxChargeTime, sec)
}

// SetCOverXThreshold_mA configures the C/x comparator threshold using RSNSB scaling.
func (li Lithium) SetCOverXThreshold_mA(mA int32) error {
	if err := li.d.ensureTargetsWritable(); err != nil {
		return err
	}
	if li.d.rsnsB_uOhm == 0 {
		return ErrRSNSBUnset
	}
	code := li.d.currCode(mA, li.d.rsnsB_uOhm)
	return li.d.writeWord(regCOverXThreshold, code)
}

func (li Lithium) EnableCOverXTermination(on bool) error {
	if on {
		return li.d.SetChargerConfigBits(EnCOverXTerm)
	}
	return li.d.ClearChargerConfigBits(EnCOverXTerm)
}

// JEITA configuration (raw NTC_RATIO thresholds).
func (li Lithium) SetJEITAThresholdsRaw(t1, t2, t3, t4, t5, t6 uint16) error {
	if err := li.d.ensureTargetsWritable(); err != nil {
		return err
	}
	if err := li.d.writeWord(regJEITAT1, t1); err != nil {
		return err
	}
	if err := li.d.writeWord(regJEITAT2, t2); err != nil {
		return err
	}
	if err := li.d.writeWord(regJEITAT3, t3); err != nil {
		return err
	}
	if err := li.d.writeWord(regJEITAT4, t4); err != nil {
		return err
	}
	if err := li.d.writeWord(regJEITAT5, t5); err != nil {
		return err
	}
	return li.d.writeWord(regJEITAT6, t6)
}

// Pack five 5-bit voltage codes for regions 2..6 into VCHARGE JEITA regs.
func (li Lithium) SetJEITAVChargeCodes(v2, v3, v4, v5, v6 uint8) error {
	if err := li.d.ensureTargetsWritable(); err != nil {
		return err
	}
	w26 := (uint16(v4&0x1F) << 10) | (uint16(v3&0x1F) << 5) | uint16(v2&0x1F)
	w25 := (uint16(v6&0x1F) << 5) | uint16(v5&0x1F)
	if err := li.d.writeWord(regJEITAVchg_2_4, w26); err != nil {
		return err
	}
	return li.d.writeWord(regJEITAVchg_5_6, w25)
}

// Pack five 5-bit current codes for regions 2..6 into ICHARGE JEITA regs.
func (li Lithium) SetJEITAChargeCurrentCodes(i2, i3, i4, i5, i6 uint8) error {
	if err := li.d.ensureTargetsWritable(); err != nil {
		return err
	}
	w28 := (uint16(i4&0x1F) << 10) | (uint16(i3&0x1F) << 5) | uint16(i2&0x1F)
	w27 := (uint16(i6&0x1F) << 5) | uint16(i5&0x1F)
	if err := li.d.writeWord(regJEITAIchg_2_4, w28); err != nil {
		return err
	}
	return li.d.writeWord(regJEITAIchg_5_6, w27)
}

// ----- LiFePO4 specifics -----

// VABSORB_DELTA: LSB = 12.5 mV/cell, bits [4:0]; 0 disables absorb.
func (lp LiFePO4) SetVAbsorbDelta_mVPerCell(delta_mV int32) error {
	if err := lp.d.ensureTargetsWritable(); err != nil {
		return err
	}
	// round(delta/12.5mV) = round(delta*1000 / 12500)
	code := (int64(delta_mV)*1000 + 6250) / 12500
	code = clampRange(code, 0, 31)
	return lp.d.writeWord(regVAbsorbDelta, uint16(code)&0x001F)
}

// MAX_ABSORB_TIME (s).
func (lp LiFePO4) SetMaxAbsorbTime_s(sec uint16) error {
	if err := lp.d.ensureTargetsWritable(); err != nil {
		return err
	}
	return lp.d.writeWord(regMaxAbsorbTime, sec)
}

// LiFePO4 recharge threshold (per-cell). LSB = 192.264 µV (lithium VBAT scaling).
func (lp LiFePO4) SetRechargeThreshold_mVPerCell(mV int32) error {
	if err := lp.d.ensureTargetsWritable(); err != nil {
		return err
	}
	num := int64(mV) * 1000
	den := int64(192264)
	code := (num + den/2) / den
	return lp.d.writeWord(regLiFePO4RchgTh, clamp16(code))
}

// ----- Lead-acid specifics -----

// VCHARGE_SETTING per-cell. Optionally cap with tempComp.
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

// MAX_ABSORB_TIME (s).
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

// Toggle temperature compensation bit in CHARGER_CONFIG_BITS.
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

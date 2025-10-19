package ltc4015

// Windows holds last-configured alert windows. Zero values mean “unspecified”.
type Windows struct {
	VinLo_mV, VinHi_mV           int32
	VsysLo_mV, VsysHi_mV         int32
	VbatLoCell_mV, VbatHiCell_mV int32
	NTCHi, NTCLo                 uint16
}

// DesiredMasks are the intended alert enables.
type DesiredMasks struct {
	Limit     LimitEnable
	ChgState  ChargerStateEnable
	ChgStatus ChargeStatusEnable
}

// ----- Limit/window setters -----

func (d *Device) SetVBATWindow_mVPerCell(lo_mV, hi_mV int32) error {
	if err := d.writeWord(regVBATLoAlertLimit, d.toVBATCode(lo_mV)); err != nil {
		return err
	}
	return d.writeWord(regVBATHiAlertLimit, d.toVBATCode(hi_mV))
}

func (d *Device) SetVINWindow_mV(lo_mV, hi_mV int32) error {
	if err := d.writeWord(regVINLoAlertLimit, d.toCode_1p648mV_LSB(lo_mV)); err != nil {
		return err
	}
	return d.writeWord(regVINHiAlertLimit, d.toCode_1p648mV_LSB(hi_mV))
}

func (d *Device) SetVSYSWindow_mV(lo_mV, hi_mV int32) error {
	if err := d.writeWord(regVSYSLoAlertLimit, d.toCode_1p648mV_LSB(lo_mV)); err != nil {
		return err
	}
	return d.writeWord(regVSYSHiAlertLimit, d.toCode_1p648mV_LSB(hi_mV))
}

func (d *Device) SetIINHigh_mA(mA int32) error {
	if d.rsnsI_uOhm == 0 {
		return ErrRSNSIUnset
	}
	return d.writeWord(regIINHiAlertLimit, currCodeUnsigned_mA(mA, d.rsnsI_uOhm))
}

func (d *Device) SetIBATLow_mA(mA int32) error {
	if d.rsnsB_uOhm == 0 {
		return ErrRSNSBUnset
	}
	return d.writeWord(regIBATLoAlertLimit, currCodeSigned_mA(mA, d.rsnsB_uOhm))
}

func (d *Device) SetDieTempHigh_mC(mC int32) error {
	raw := int64(12010) + (int64(456)*int64(mC))/10000
	return d.writeWord(regDieTempHiAlertLimit, clamp16(raw))
}

func (d *Device) SetBSRHigh_uOhmPerCell(uOhm uint32) error {
	if d.rsnsB_uOhm == 0 {
		return ErrRSNSBUnset
	}
	div := int64(500)
	if d.chem == ChemLeadAcid {
		div = 750
	}
	raw := (int64(uOhm) * div) / int64(d.rsnsB_uOhm)
	return d.writeWord(regBSRHiAlertLimit, clamp16(raw))
}

func (d *Device) SetNTCRatioWindowRaw(hi, lo uint16) error {
	if err := d.writeWord(regNTCRatioHiAlertLimit, hi); err != nil {
		return err
	}
	return d.writeWord(regNTCRatioLoAlertLimit, lo)
}

// ----- Convenience: *AndClear helpers (retained) -----

func (d *Device) SetVINWindowAndClear(lo_mV, hi_mV int32) error {
	if err := d.SetVINWindow_mV(lo_mV, hi_mV); err != nil {
		return err
	}
	return d.ClearLimitAlerts()
}

func (d *Device) SetVSYSWindowAndClear(lo_mV, hi_mV int32) error {
	if err := d.SetVSYSWindow_mV(lo_mV, hi_mV); err != nil {
		return err
	}
	return d.ClearLimitAlerts()
}

func (d *Device) SetVBATWindowPerCellAndClear(lo_mV, hi_mV int32) error {
	if err := d.SetVBATWindow_mVPerCell(lo_mV, hi_mV); err != nil {
		return err
	}
	return d.ClearLimitAlerts()
}

func (d *Device) SetNTCRatioWindowAndClear(hi, lo uint16) error {
	if err := d.SetNTCRatioWindowRaw(hi, lo); err != nil {
		return err
	}
	return d.ClearLimitAlerts()
}

// ----- Enables / Reads / Clears -----

func (d *Device) EnableLimitAlertsMask(mask LimitEnable) error {
	return d.writeWord(regEnLimitAlerts, uint16(mask))
}
func (d *Device) EnableChargerStateAlertsMask(mask ChargerStateEnable) error {
	return d.writeWord(regEnChargerStAlerts, uint16(mask))
}
func (d *Device) EnableChargeStatusAlertsMask(mask ChargeStatusEnable) error {
	return d.writeWord(regEnChargeStAlerts, uint16(mask))
}

func (d *Device) ReadLimitAlerts() (LimitAlerts, error) {
	v, err := d.readWord(regLimitAlerts)
	return LimitAlerts(v), err
}
func (d *Device) ReadChargerStateAlerts() (ChargerStateAlerts, error) {
	v, err := d.readWord(regChargerStateAlert)
	return ChargerStateAlerts(v), err
}
func (d *Device) ReadChargeStatusAlerts() (ChargeStatusAlerts, error) {
	v, err := d.readWord(regChargeStatAlerts)
	return ChargeStatusAlerts(v), err
}

func (d *Device) ClearLimitAlerts() error        { return d.writeWord(regLimitAlerts, 0x0000) }
func (d *Device) ClearChargerStateAlerts() error { return d.writeWord(regChargerStateAlert, 0x0000) }
func (d *Device) ClearChargeStatusAlerts() error { return d.writeWord(regChargeStatAlerts, 0x0000) }

// ----- Rearm strategy -----

// RearmOppositeEdges refines windowed limit masks to arm the opposite edge,
// prunes already-asserted bits, writes enable masks and clears latches.
// Best-effort: read errors do not abort other groups; returns first write error.
func (d *Device) RearmOppositeEdges(desired DesiredMasks, last Windows) error {
	var firstWriteErr error
	setWriteErr := func(err error) {
		if err != nil && firstWriteErr == nil {
			firstWriteErr = err
		}
	}

	// Charge Status
	if cur, err := d.ChargeStatus(); err == nil {
		setWriteErr(d.EnableChargeStatusAlertsMask(desired.ChgStatus &^ cur))
		setWriteErr(d.ClearChargeStatusAlerts())
	}

	// Charger State
	if cur, err := d.ChargerState(); err == nil {
		setWriteErr(d.EnableChargerStateAlertsMask(desired.ChgState &^ cur))
		setWriteErr(d.ClearChargerStateAlerts())
	}

	// Limits
	en := desired.Limit

	// VIN refine
	if last.VinLo_mV != 0 || last.VinHi_mV != 0 {
		if mv, err := d.Vin_mV(); err == nil {
			switch {
			case mv >= last.VinHi_mV:
				en &^= VINHi
				en |= VINLo
			case mv <= last.VinLo_mV:
				en &^= VINLo
				en |= VINHi
			default:
				en |= VINLo | VINHi
			}
		} else {
			en |= VINLo | VINHi
		}
	}

	// VSYS refine
	if last.VsysLo_mV != 0 || last.VsysHi_mV != 0 {
		if mv, err := d.Vsys_mV(); err == nil {
			switch {
			case mv >= last.VsysHi_mV:
				en &^= VSYSHi
				en |= VSYSLo
			case mv <= last.VsysLo_mV:
				en &^= VSYSLo
				en |= VSYSHi
			default:
				en |= VSYSLo | VSYSHi
			}
		} else {
			en |= VSYSLo | VSYSHi
		}
	}

	// VBAT (per-cell) refine
	if last.VbatLoCell_mV != 0 || last.VbatHiCell_mV != 0 {
		if per, err := d.Battery_mVPerCell(); err == nil {
			switch {
			case per >= last.VbatHiCell_mV:
				en &^= VBATHi
				en |= VBATLo
			case per <= last.VbatLoCell_mV:
				en &^= VBATLo
				en |= VBATHi
			default:
				en |= VBATLo | VBATHi
			}
		} else {
			en |= VBATLo | VBATHi
		}
	}

	// NTC: enable both if window configured
	if last.NTCHi != 0 || last.NTCLo != 0 {
		en |= NTCRatioLo | NTCRatioHi
	}

	// Prune asserted
	if asserted, err := d.ReadLimitAlerts(); err == nil {
		en &^= asserted
	}

	// Apply enables and clear latches
	setWriteErr(d.EnableLimitAlertsMask(en))
	setWriteErr(d.ClearLimitAlerts())

	return firstWriteErr
}

// ----- SMBus alert service -----

// AlertEvent summarises latched alert sources read from the device.
type AlertEvent struct {
	Limit     LimitAlerts        // 0x36: LIMIT_ALERTS
	ChgState  ChargerStateAlerts // 0x37: CHARGER_STATE_ALERTS
	ChgStatus ChargeStatusAlerts // 0x38: CHARGE_STATUS_ALERTS
}

func (e AlertEvent) Empty() bool {
	return e.Limit == 0 && e.ChgState == 0 && e.ChgStatus == 0
}

// ServiceSMBAlert performs ARA and then drains/clears alert latches.
// Returns (event, true, nil) if LTC4015 identified itself; (zero, false, nil) if not.
func (d *Device) ServiceSMBAlert() (AlertEvent, bool, error) {
	ok, err := d.AcknowledgeAlert()
	if err != nil || !ok {
		return AlertEvent{}, false, err
	}
	ev, err := d.DrainAlerts()
	return ev, true, err
}

// DrainAlerts reads the three alert groups and then issues clear writes.
func (d *Device) DrainAlerts() (AlertEvent, error) {
	var ev AlertEvent

	lim, err := d.ReadLimitAlerts()
	if err != nil {
		return AlertEvent{}, err
	}
	csa, err := d.ReadChargerStateAlerts()
	if err != nil {
		return AlertEvent{}, err
	}
	css, err := d.ReadChargeStatusAlerts()
	if err != nil {
		return AlertEvent{}, err
	}

	ev.Limit = lim
	ev.ChgState = csa
	ev.ChgStatus = css

	// Best-effort clears; do not mask the event on clear failures.
	var clearErr error
	if err := d.ClearLimitAlerts(); err != nil {
		clearErr = err
	}
	if err := d.ClearChargerStateAlerts(); err != nil && clearErr == nil {
		clearErr = err
	}
	if err := d.ClearChargeStatusAlerts(); err != nil && clearErr == nil {
		clearErr = err
	}
	return ev, clearErr
}

// ----- Coulomb counter (minimal) -----

func (d *Device) SetQCount(v uint16) error { return d.writeWord(regQCount, v) }
func (d *Device) QCount() (uint16, error)  { return d.readWord(regQCount) }
func (d *Device) SetQCountLimits(lo, hi uint16) error {
	if err := d.writeWord(regQCountLoLimit, lo); err != nil {
		return err
	}
	return d.writeWord(regQCountHiLimit, hi)
}
func (d *Device) SetQCountPrescale(p uint16) error { return d.writeWord(regQCountPrescale, p) }

// ----- Input parameter setting -----

// Input current limit: (code+1)*500 µV across RSNSI, 0..63.
func (d *Device) SetIinLimit_mA(mA int32) error {
	if d.rsnsI_uOhm == 0 {
		return ErrRSNSIUnset
	}
	v_uV := (int64(mA) * int64(d.rsnsI_uOhm)) / 1000 // µV across RSNSI
	code := qLinear(v_uV, 500 /*µV*/, 0, true, 0, 63)
	return d.writeWord(regIinLimitSetting, code)
}

// VIN_UVCL: (code+1)*4.6875 mV, 0..255 (nanovolts avoid fractional LSB).
func (d *Device) SetVinUvcl_mV(mV int32) error {
	v_nV := int64(mV) * 1_000_000
	step_nV := int64(4_687_500)
	code := qLinear(v_nV, step_nV, 0, true, 0, 255)
	return d.writeWord(regVinUvclSetting, code)
}

// Charge parameter setting (programmable variants only).

// ICHARGE_TARGET: (code+1)*1 mV across RSNSB, 0..31.
func (d *Device) SetIChargeTarget_mA(mA int32) error {
	if err := d.ensureTargetsWritable(); err != nil {
		return err
	}
	if d.rsnsB_uOhm == 0 {
		return ErrRSNSBUnset
	}
	v_uV := (int64(mA) * int64(d.rsnsB_uOhm)) / 1000 // µV across RSNSB
	code := qLinear(v_uV, 1000 /*µV*/, 0, true, 0, 31)
	return d.writeWord(regIChargeTarget, code)
}

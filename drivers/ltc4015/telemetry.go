package ltc4015

// Voltages

func (d *Device) Battery_mVPerCell() (int32, error) {
	raw, err := d.readWord(regVBAT)
	if err != nil {
		return 0, err
	}
	// Li: 192,264 nV/LSB; Lead: 128,176 nV/LSB.
	nV := int64(192264)
	if d.chem == ChemLeadAcid {
		nV = 128176
	}
	uV := (int64(raw) * nV) / 1000 // nV → µV
	return int32(uV / 1000), nil   // µV → mV
}

func (d *Device) Battery_mVPack() (int32, error) {
	perCell, err := d.Battery_mVPerCell()
	if err != nil {
		return 0, err
	}
	if d.cells == 0 {
		return perCell, nil
	}
	return perCell * int32(d.cells), nil
}

func (d *Device) Vin_mV() (int32, error) {
	raw, err := d.readWord(regVIN)
	if err != nil {
		return 0, err
	}
	uV := int64(raw) * 1648
	return int32(uV / 1000), nil
}

func (d *Device) Vsys_mV() (int32, error) {
	raw, err := d.readWord(regVSYS)
	if err != nil {
		return 0, err
	}
	uV := int64(raw) * 1648
	return int32(uV / 1000), nil
}

// Currents

func (d *Device) Ibat_mA() (int32, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, ErrRSNSBUnset
	}
	raw, err := d.readS16(regIBAT)
	if err != nil {
		return 0, err
	}
	uA := (int64(raw) * 1464870) / int64(d.rsnsB_uOhm)
	return int32(uA / 1000), nil
}

func (d *Device) Iin_mA() (int32, error) {
	if d.rsnsI_uOhm == 0 {
		return 0, ErrRSNSIUnset
	}
	raw, err := d.readS16(regIIN)
	if err != nil {
		return 0, err
	}
	uA := (int64(raw) * 1464870) / int64(d.rsnsI_uOhm)
	return int32(uA / 1000), nil
}

// Temperature

func (d *Device) Die_mC() (int32, error) {
	raw, err := d.readS16(regDieTemp)
	if err != nil {
		return 0, err
	}
	return int32((int64(raw) - 12010) * 10000 / 456), nil
}

// Battery series resistance proxy

func (d *Device) BSR_uOhmPerCell() (uint32, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, ErrRSNSBUnset
	}
	raw, err := d.readWord(regBSR)
	if err != nil {
		return 0, err
	}
	div := int64(500)
	if d.chem == ChemLeadAcid {
		div = 750
	}
	return uint32((int64(raw) * int64(d.rsnsB_uOhm)) / div), nil
}

// Measurement validity

func (d *Device) MeasSystemValid() (bool, error) {
	v, err := d.readWord(regMeasSysValid)
	if err != nil {
		return false, err
	}
	return (v & 0x0001) != 0, nil
}

// ICHARGE_BSR (IBAT used for BSR calc) exposed in mA

func (d *Device) IChargeBSR_mA() (int32, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, ErrRSNSBUnset
	}
	raw, err := d.readS16(regIChargeBSR)
	if err != nil {
		return 0, err
	}
	uA := (int64(raw) * 1464870) / int64(d.rsnsB_uOhm)
	return int32(uA / 1000), nil
}

// Raw NTC ratio.

func (d *Device) NTCRatio() (uint16, error) {
	v, err := d.readWord(regNTCRatio)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// Effective DAC read-backs and convenience in physical units.

func (d *Device) IChargeDACCode() (uint16, error)  { return d.readWord(regIChargeDAC) }
func (d *Device) VChargeDACCode() (uint16, error)  { return d.readWord(regVChargeDAC) }
func (d *Device) IinLimitDACCode() (uint16, error) { return d.readWord(regIinLimitDAC) }

func (d *Device) IChargeDAC_mA() (int32, error) {
	if d.rsnsB_uOhm == 0 {
		return 0, ErrRSNSBUnset
	}
	code, err := d.IChargeDACCode()
	if err != nil {
		return 0, err
	}
	// I = ((code+1)*1 mV)/RSNSB
	mA := (int64(code) + 1) * 1_000_000 / int64(d.rsnsB_uOhm)
	return int32(mA), nil
}

func (d *Device) IinLimitDAC_mA() (int32, error) {
	if d.rsnsI_uOhm == 0 {
		return 0, ErrRSNSIUnset
	}
	code, err := d.IinLimitDACCode()
	if err != nil {
		return 0, err
	}
	// I = ((code+1)*0.5 mV)/RSNSI
	mA := (int64(code) + 1) * 500_000 / int64(d.rsnsI_uOhm)
	return int32(mA), nil
}

// Typed status readers.

func (d *Device) SystemStatus() (SystemStatus, error) {
	v, err := d.readWord(regSystemStatus)
	return SystemStatus(v), err
}

func (d *Device) ChargerState() (ChargerState, error) {
	v, err := d.readWord(regChargerState)
	return ChargerState(v), err
}

func (d *Device) ChargeStatus() (ChargeStatus, error) {
	v, err := d.readWord(regChargeStatus)
	return ChargeStatus(v), err
}

// Timer read-backs.

func (d *Device) CVTimer_s() (uint16, error)       { return d.readWord(regCVTimer) }
func (d *Device) AbsorbTimer_s() (uint16, error)   { return d.readWord(regAbsorbTimer) }
func (d *Device) EqualizeTimer_s() (uint16, error) { return d.readWord(regEqualizeTimer) }

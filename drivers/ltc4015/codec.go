package ltc4015

// Clamp helpers.

func clampRange(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp16(v int64) uint16 {
	return uint16(int16(clampRange(v, -32768, 32767)))
}

func clamp16u(v int64) uint16 {
	if v < 0 {
		return 0
	}
	if v > 0xFFFF {
		return 0xFFFF
	}
	return uint16(v)
}

// qLinear maps a physical value onto a linear code:
//
//	code_physical = (code + addOne?1:0)*step + offset
//
// inverse:
//
//	code = round((value - offset)/step) - (addOne?1:0)
func qLinear(value, step, offset int64, addOne bool, lo, hi int64) uint16 {
	num := value - offset
	if num < 0 {
		num = 0
	}
	code := (num + step/2) / step
	if addOne && code > 0 {
		code--
	}
	code = clampRange(code, lo, hi)
	return uint16(code)
}

// Lead-acid voltage-count mapping for VCHARGE_SETTING and deltas.
// Vcell ≈ 2000 mV + code*(1000/105 mV)
// Therefore: code ≈ round(105*(mV - offset_mV)/1000).
func laCountsFrommV(mV, offset_mV int32) int64 {
	return (int64(mV-offset_mV)*105 + 500) / 1000
}

// Unit conversions bound to device chemistry/config.

func (d *Device) toVBATCode(mV int32) uint16 {
	nV := int64(192264)
	if d.chem == ChemLeadAcid {
		nV = 128176
	}
	code := (int64(mV)*1_000_000 + nV/2) / nV
	return clamp16u(code)
}

func (d *Device) toCode_1p648mV_LSB(mV int32) uint16 {
	const nV = 1_648_000
	code := (int64(mV)*1_000_000 + nV/2) / nV
	return clamp16u(code)
}

func (d *Device) currCode(mA int32, rsns_uOhm uint32) uint16 {
	const pVperLSB = 1_464_870
	uA := int64(mA) * 1000
	code := (uA*int64(rsns_uOhm) + pVperLSB/2) / pVperLSB
	return clamp16(code)
}

func currCodeUnsigned_mA(mA int32, rsns_uOhm uint32) uint16 {
	const pVperLSB = 1_464_870
	uA := int64(mA) * 1000
	if uA < 0 {
		uA = 0
	}
	code := (uA*int64(rsns_uOhm) + pVperLSB/2) / pVperLSB
	return clamp16u(code)
}

func currCodeSigned_mA(mA int32, rsns_uOhm uint32) uint16 {
	const pVperLSB = 1_464_870
	uA := int64(mA) * 1000
	code := (uA*int64(rsns_uOhm) + pVperLSB/2) / pVperLSB
	return clamp16(code)
}

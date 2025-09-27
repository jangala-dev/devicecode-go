package mathx

// MapU16 maps x in [inMin,inMax] to [outMin,outMax] with 32-bit intermediates.
// Clamps to out range if input is outside.
func MapU16(x, inMin, inMax, outMin, outMax uint16) uint16 {
	if inMax == inMin {
		return outMin
	}
	// Clamp input first to avoid over/underflow in multiply.
	if x < inMin {
		return outMin
	}
	if x > inMax {
		return outMax
	}
	num := uint32(x-inMin) * uint32(outMax-outMin)
	den := uint32(inMax - inMin)
	return uint16(uint32(outMin) + num/den)
}

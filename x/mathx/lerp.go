package mathx

// LerpU16 returns linear interpolation between a and b, with t in [0..65535] (Q16).
// Result is in [min(a,b), max(a,b)].
func LerpU16(a, b, t uint16) uint16 {
	// Equivalent to a + ((b-a)*t)/65535 with 32-bit intermediates.
	da := int32(b) - int32(a)
	res := int32(a) + (da*int32(t))/65535
	if res < 0 {
		return 0
	}
	if res > 65535 {
		return 65535
	}
	return uint16(res)
}

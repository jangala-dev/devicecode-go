package conv

// Itoa writes base-10 representation of n into buf and returns the used slice.
// buf should be length >= 20 for int64. Negative numbers supported.
// No allocations; no fmt/strconv dependency.
func Itoa(buf []byte, n int64) []byte {
	if len(buf) == 0 {
		return buf[:0]
	}
	i := len(buf)
	neg := n < 0
	var u uint64
	if neg {
		u = uint64(-n)
	} else {
		u = uint64(n)
	}
	// Write digits backwards.
	if u == 0 {
		i--
		buf[i] = '0'
	} else {
		for u > 0 && i > 0 {
			i--
			buf[i] = byte('0' + (u % 10))
			u /= 10
		}
	}
	if neg && i > 0 {
		i--
		buf[i] = '-'
	}
	return buf[i:]
}

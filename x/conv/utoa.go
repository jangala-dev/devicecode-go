package conv

// Utoa writes base-10 representation of n into buf and returns the used slice.
// buf should be length >= 20 for uint64.
func Utoa(buf []byte, n uint64) []byte {
	if len(buf) == 0 {
		return buf[:0]
	}
	i := len(buf)
	if n == 0 {
		i--
		buf[i] = '0'
	} else {
		for n > 0 && i > 0 {
			i--
			buf[i] = byte('0' + (n % 10))
			n /= 10
		}
	}
	return buf[i:]
}

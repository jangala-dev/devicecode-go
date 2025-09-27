package conv

// U32Hex writes 8-digit uppercase hex without 0x, zero-padded.
func U32Hex(buf []byte, n uint32) []byte {
	if len(buf) < 8 {
		return buf[:0]
	}
	const hexd = "0123456789ABCDEF"
	i := len(buf)
	for j := 0; j < 8; j++ {
		i--
		buf[i] = hexd[n&0xF]
		n >>= 4
	}
	return buf[i:]
}

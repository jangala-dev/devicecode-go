//go:build mcu

package strconvx

// Minimal, allocation-aware helpers with identical signatures.
// Supported bases: 2..36 for Format* and Parse*.
// FormatFloat/ParseFloat are basic and not IEEE-perfect; use sparingly on MCU.

func Itoa(i int) string { return FormatInt(int64(i), 10) }

func Atoi(s string) (int, error) {
	v, err := ParseInt(s, 10, 0)
	return int(v), err
}

func FormatInt(i int64, base int) string {
	if base < 2 || base > 36 {
		base = 10
	}
	neg := i < 0
	var u uint64
	if neg {
		u = uint64(-i)
	} else {
		u = uint64(i)
	}
	s := formatUint(u, base)
	if neg {
		return "-" + s
	}
	return s
}

func FormatUint(u uint64, base int) string {
	if base < 2 || base > 36 {
		base = 10
	}
	return formatUint(u, base)
}

func formatUint(u uint64, base int) string {
	if u == 0 {
		return "0"
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf [64]byte
	i := len(buf)
	b := uint64(base)
	for u > 0 {
		i--
		buf[i] = digits[u%b]
		u /= b
	}
	return string(buf[i:])
}

type parseError struct{}

func (parseError) Error() string { return "invalid syntax" }

// bitSize: 0,8,16,32,64 like strconv. 0 => int size; map to 64 here.
func ParseInt(s string, base, bitSize int) (int64, error) {
	// 1) Strip sign first.
	neg := false
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}

	// 2) Auto-detect base on unsigned part if requested.
	if base == 0 {
		base = detectBase(&s)
	}

	// 3) Parse as unsigned, then apply sign with range checks.
	u, err := ParseUint(s, base, 64)
	if err != nil {
		return 0, err
	}
	if neg {
		if u > 1<<63 {
			return 0, parseError{}
		}
		return -int64(u), nil
	}
	if u >= 1<<63 {
		// Disallow values outside int64 positive range.
		if !(bitSize == 64 && u == 1<<63) {
			return 0, parseError{}
		}
	}
	return int64(u), nil
}

func ParseUint(s string, base, bitSize int) (uint64, error) {
	if base == 0 {
		base = detectBase(&s)
	}
	if base < 2 || base > 36 || len(s) == 0 {
		return 0, parseError{}
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d byte
		switch {
		case '0' <= c && c <= '9':
			d = c - '0'
		case 'a' <= c && c <= 'z':
			d = c - 'a' + 10
		case 'A' <= c && c <= 'Z':
			d = c - 'A' + 10
		default:
			return 0, parseError{}
		}
		if int(d) >= base {
			return 0, parseError{}
		}
		v = v*uint64(base) + uint64(d)
	}
	// Truncate to requested bitSize range.
	switch bitSize {
	case 0, 64:
		return v, nil
	case 8:
		return v & ((1 << 8) - 1), nil
	case 16:
		return v & ((1 << 16) - 1), nil
	case 32:
		return v & ((1 << 32) - 1), nil
	default:
		return v, nil
	}
}

func detectBase(ps *string) int {
	s := *ps
	if len(s) >= 2 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			*ps = s[2:]
			return 16
		case 'b', 'B':
			*ps = s[2:]
			return 2
		case 'o', 'O':
			*ps = s[2:]
			return 8
		}
	}
	return 10
}

// Basic float formatting/parsing for MCU.
// Keep expectations modest: no infinities/NaN formatting, minimal precision.

func FormatFloat(f float64, fmt byte, prec, _ int) string {
	// Support only 'f' and 'g' basic forms; switch to decimal with given precision.
	if fmt != 'f' && fmt != 'g' && fmt != 'e' && fmt != 'E' {
		fmt = 'f'
	}
	if prec < 0 {
		prec = 6
	}
	neg := false
	if f < 0 {
		neg = true
		f = -f
	}
	intp := uint64(f)
	frac := f - float64(intp)

	ints := FormatUint(intp, 10)
	if prec == 0 {
		if neg {
			return "-" + ints
		}
		return ints
	}
	// Multiply fractional part.
	pow := 1.0
	for i := 0; i < prec; i++ {
		pow *= 10
	}
	fracN := uint64(frac*pow + 0.5) // simple rounding
	fs := FormatUint(fracN, 10)
	// zero-pad fractional
	if len(fs) < prec {
		z := make([]byte, prec-len(fs))
		for i := range z {
			z[i] = '0'
		}
		fs = string(z) + fs
	}
	out := ints + "." + fs
	if neg {
		return "-" + out
	}
	return out
}

func ParseFloat(s string, _ int) (float64, error) {
	if len(s) == 0 {
		return 0, parseError{}
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
	}
	var intPart uint64
	var i int
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		intPart = intPart*10 + uint64(s[i]-'0')
		i++
	}
	var frac float64
	if i < len(s) && s[i] == '.' {
		i++
		scale := 1.0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			frac = frac*10 + float64(s[i]-'0')
			scale *= 10
			i++
		}
		frac = frac / scale
	}
	if i != len(s) {
		return 0, parseError{}
	}
	v := float64(intPart) + frac
	if neg {
		v = -v
	}
	return v, nil
}

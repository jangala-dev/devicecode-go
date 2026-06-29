//go:build !(rp2040 || rp2350)

package strconvx

import "strconv"

// Mirror the MCU helpers on host builds so tests exercise the same parsing
// rules, including base-0 prefix handling for 0b/0o/0x inputs.

func Itoa(i int) string                    { return strconv.Itoa(i) }
func Itoa64(i int64) string                { return strconv.FormatInt(i, 10) }
func Utoa64(u uint64) string               { return strconv.FormatUint(u, 10) }
func Atoi(s string) (int, error)           { return strconv.Atoi(s) }
func FormatInt(i int64, base int) string   { return strconv.FormatInt(i, base) }
func FormatUint(u uint64, base int) string { return strconv.FormatUint(u, base) }
func ParseInt(s string, base, bitSize int) (int64, error) {
	if base != 0 {
		return strconv.ParseInt(s, base, bitSize)
	}
	neg := false
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	base = detectBase(&s)
	if neg {
		s = "-" + s
	}
	return strconv.ParseInt(s, base, bitSize)
}
func ParseUint(s string, base, bitSize int) (uint64, error) {
	if base == 0 {
		base = detectBase(&s)
	}
	return strconv.ParseUint(s, base, bitSize)
}
func FormatFloat(f float64, fmt byte, prec, bitSize int) string {
	return strconv.FormatFloat(f, fmt, prec, bitSize)
}
func ParseFloat(s string, bitSize int) (float64, error) { return strconv.ParseFloat(s, bitSize) }

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

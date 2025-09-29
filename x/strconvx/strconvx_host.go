//go:build !rp2040

package strconvx

import "strconv"

// The goal is signature parity with strconv.
// Delegate straight through.

func Itoa(i int) string                                   { return strconv.Itoa(i) }
func Atoi(s string) (int, error)                          { return strconv.Atoi(s) }
func FormatInt(i int64, base int) string                  { return strconv.FormatInt(i, base) }
func FormatUint(u uint64, base int) string                { return strconv.FormatUint(u, base) }
func ParseInt(s string, base, bitSize int) (int64, error) { return strconv.ParseInt(s, base, bitSize) }
func ParseUint(s string, base, bitSize int) (uint64, error) {
	return strconv.ParseUint(s, base, bitSize)
}
func FormatFloat(f float64, fmt byte, prec, bitSize int) string {
	return strconv.FormatFloat(f, fmt, prec, bitSize)
}
func ParseFloat(s string, bitSize int) (float64, error) { return strconv.ParseFloat(s, bitSize) }

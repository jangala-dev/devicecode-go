package mathx

import "golang.org/x/exp/constraints"

// Clamp limits v to [lo, hi]. If lo > hi, the bounds are swapped.
func Clamp[T constraints.Ordered](v, lo, hi T) T {
	if hi < lo {
		lo, hi = hi, lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Between reports lo <= v && v <= hi (order-insensitive).
func Between[T constraints.Ordered](v, lo, hi T) bool {
	if hi < lo {
		lo, hi = hi, lo
	}
	return v >= lo && v <= hi
}

// Min/Max for convenience.
func Min[T constraints.Ordered](a, b T) T {
	if a < b {
		return a
	}
	return b
}
func Max[T constraints.Ordered](a, b T) T {
	if a > b {
		return a
	}
	return b
}

// Abs for signed integers.
func Abs[T ~int | ~int8 | ~int16 | ~int32 | ~int64](x T) T {
	if x < 0 {
		return -x
	}
	return x
}

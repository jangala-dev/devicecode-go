package mathx

// CeilDiv returns ceil(a/b) for positive integers.
// For non-positive inputs, behaviour is implementation-defined – keep to positives for firmware maths.
func CeilDiv[T ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64](a, b T) T {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}

// RoundDiv returns floor((a + b/2)/b), classic rounding for positives.
func RoundDiv[T ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64](a, b T) T {
	if b == 0 {
		return 0
	}
	return (a + b/2) / b
}

package core

import "devicecode-go/errcode"

// As[T] asserts a payload to the concrete value type T.
// Pointers are not accepted. A nil payload is treated as the zero value of T.
func As[T any](v any) (T, errcode.Code) {
	var zero T
	if v == nil {
		return zero, ""
	}
	t, ok := v.(T)
	if !ok {
		return zero, errcode.InvalidPayload
	}
	return t, ""
}

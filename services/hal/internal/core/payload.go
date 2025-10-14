package core

import "devicecode-go/errcode"

// As[T] asserts a payload to the concrete value type T.
// Accepts either a value (T) or a pointer (*T). A nil payload is treated as the zero value of T.
func As[T any](v any) (T, errcode.Code) {
	var zero T
	if v == nil {
		return zero, ""
	}
	if t, ok := v.(T); ok {
		return t, ""
	}
	if pt, ok := v.(*T); ok && pt != nil {
		return *pt, ""
	}
	return zero, errcode.InvalidPayload
}

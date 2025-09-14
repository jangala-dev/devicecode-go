// services/hal/internal/util/util.go
package util

import (
	"encoding/json"
	"fmt"
	"time"
)

func ResetTimer(t *time.Timer, d time.Duration) {
	if d < 0 {
		d = 0
	}
	if !t.Stop() {
		DrainTimer(t)
	}
	t.Reset(d)
}

func DrainTimer(t *time.Timer) {
	select {
	case <-t.C:
	default:
	}
}

func DecodeJSON[T any](src any, dst *T) error {
	switch v := src.(type) {
	case []byte:
		return json.Unmarshal(v, dst)
	case string:
		return json.Unmarshal([]byte(v), dst)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, dst)
	}
}

func Errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

func BoolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ClampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

package integration

import (
	"context"
	"devicecode-go/bus"
	"time"
)

func recvOrTimeout(ch <-chan *bus.Message, d time.Duration) (*bus.Message, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case m := <-ch:
		return m, nil
	case <-timer.C:
		return nil, context.DeadlineExceeded
	}
}

func asInt(t any) (int, bool) {
	switch v := t.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// tiny itoa to avoid fmt in this test target
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	sign := ""
	if i < 0 {
		sign = "-"
		i = -i
	}
	var buf [32]byte
	b := len(buf)
	for i > 0 {
		b--
		buf[b] = byte('0' + (i % 10))
		i /= 10
	}
	if sign != "" {
		b--
		//nolint:staticcheck // tiny helper, fine
		buf[b] = '-'
	}
	return string(buf[b:])
}

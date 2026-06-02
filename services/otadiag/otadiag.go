package otadiag

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const XferNone = "-"

type Field struct {
	Key   string
	Value string
}

var (
	startedAt = time.Now()
	nextSeq   atomic.Uint64

	sinkMu sync.Mutex
	sink   func(string)

	windowMu     sync.Mutex
	windowStop   chan struct{}
	windowXferID string
	updaterSnap  StageSnapshot
)

type StageSnapshot struct {
	State       string
	Generation  uint64
	LeaseActive bool
	XferID      string
}

func KV(key string, value any) Field {
	return Field{Key: key, Value: valueString(value)}
}

func Event(prefix, event, xferID string, fields ...Field) {
	if xferID == "" {
		xferID = XferNone
	}
	seq := nextSeq.Add(1)
	uptime := time.Since(startedAt).Milliseconds()

	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(" seq ")
	b.WriteString(strconv.FormatUint(seq, 10))
	b.WriteString(" uptime_ms ")
	b.WriteString(strconv.FormatInt(uptime, 10))
	b.WriteString(" xfer_id ")
	b.WriteString(xferID)
	b.WriteString(" ev ")
	b.WriteString(event)
	for _, f := range fields {
		if f.Key == "" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(f.Key)
		b.WriteByte(' ')
		b.WriteString(f.Value)
	}
	emit(b.String())
}

func SetSinkForTest(fn func(string)) func() {
	sinkMu.Lock()
	prev := sink
	sink = fn
	sinkMu.Unlock()
	return func() {
		sinkMu.Lock()
		sink = prev
		sinkMu.Unlock()
	}
}

func SetUpdaterSnapshot(s StageSnapshot) {
	if s.XferID == "" {
		s.XferID = XferNone
	}
	windowMu.Lock()
	updaterSnap = s
	windowMu.Unlock()
}

func StartUpdateWindow(reason, xferID string) {
	if xferID == "" {
		xferID = XferNone
	}
	windowMu.Lock()
	if windowStop != nil {
		close(windowStop)
	}
	stop := make(chan struct{})
	windowStop = stop
	windowXferID = xferID
	windowMu.Unlock()

	Event("[mcu-ota]", "heartbeat_start", xferID, KV("reason", reason))
	go heartbeatLoop(stop)
}

func SetActiveXfer(xferID string) {
	if xferID == "" {
		xferID = XferNone
	}
	windowMu.Lock()
	windowXferID = xferID
	if updaterSnap.XferID == "" || updaterSnap.XferID == XferNone {
		updaterSnap.XferID = xferID
	}
	windowMu.Unlock()
}

func StopUpdateWindow(reason string) {
	stopUpdateWindow(nil, reason)
}

func stopUpdateWindow(expected <-chan struct{}, reason string) {
	xferID := XferNone
	stopped := false
	windowMu.Lock()
	if windowXferID != "" {
		xferID = windowXferID
	}
	if windowStop != nil && (expected == nil || expected == windowStop) {
		close(windowStop)
		windowStop = nil
		windowXferID = XferNone
		stopped = true
	}
	windowMu.Unlock()
	if stopped {
		Event("[mcu-ota]", "heartbeat_stop", xferID, KV("reason", reason))
	}
}

func WindowActive() bool {
	windowMu.Lock()
	active := windowStop != nil
	windowMu.Unlock()
	return active
}

func heartbeatLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()

	for {
		select {
		case <-stop:
			return
		case <-deadline.C:
			stopUpdateWindow(stop, "heartbeat_deadline")
			return
		case <-ticker.C:
			xferID, snap := heartbeatSnapshot()
			Event(
				"[mcu-ota]", "heartbeat", xferID,
				KV("state", blank(snap.State)),
				KV("generation", snap.Generation),
				KV("lease_active", snap.LeaseActive),
				KV("active_xfer", blank(snap.XferID)),
			)
		}
	}
}

func heartbeatSnapshot() (string, StageSnapshot) {
	windowMu.Lock()
	defer windowMu.Unlock()
	xferID := windowXferID
	if xferID == "" || xferID == XferNone {
		xferID = updaterSnap.XferID
	}
	if xferID == "" {
		xferID = XferNone
	}
	return xferID, updaterSnap
}

func emit(line string) {
	sinkMu.Lock()
	fn := sink
	sinkMu.Unlock()
	if fn != nil {
		fn(line)
		return
	}
	println(line)
}

func blank(v string) string {
	if v == "" {
		return XferNone
	}
	return v
}

func valueString(v any) string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return XferNone
		}
		return compact(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint:
		return strconv.FormatUint(uint64(x), 10)
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case time.Duration:
		return strconv.FormatInt(x.Milliseconds(), 10)
	default:
		return XferNone
	}
}

func compact(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' {
			b.WriteByte('_')
			continue
		}
		b.WriteByte(c)
	}
	if b.Len() == 0 {
		return XferNone
	}
	return b.String()
}

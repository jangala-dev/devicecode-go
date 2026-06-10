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
	startedAt           = time.Now()
	nextSeq             atomic.Uint64
	verbose             atomic.Bool
	heartbeatDeadlineMS atomic.Int64

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

// Enabled reports whether Event would emit a line for prefix/event under the
// current verbosity policy. It is used by TinyGo hot paths to avoid building
// diagnostic Field values that would be filtered out.
func Enabled(prefix, event string) bool {
	return allowEvent(prefix, event, nil)
}

func Event(prefix, event, xferID string, fields ...Field) {
	if !allowEvent(prefix, event, fields) {
		return
	}
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
	prevVerbose := verbose.Load()
	sink = fn
	verbose.Store(true)
	sinkMu.Unlock()
	return func() {
		sinkMu.Lock()
		sink = prev
		verbose.Store(prevVerbose)
		sinkMu.Unlock()
	}
}

func SetVerboseForTest(enabled bool) func() {
	prev := verbose.Load()
	verbose.Store(enabled)
	return func() {
		verbose.Store(prev)
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

func SetHeartbeatDeadline(d time.Duration) {
	if d <= 0 {
		d = 45 * time.Second
	}
	heartbeatDeadlineMS.Store(d.Milliseconds())
}

func currentHeartbeatDeadline() time.Duration {
	ms := heartbeatDeadlineMS.Load()
	if ms <= 0 {
		return 45 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
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
	deadline := time.NewTimer(currentHeartbeatDeadline())
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

func allowEvent(prefix, event string, fields []Field) bool {
	if verbose.Load() {
		return true
	}
	// Normal firmware builds keep Fabric/OTA observability as retained counters
	// and state, not as per-frame/per-chunk log lines. The sink used by tests can
	// still opt into the detailed stream through SetSinkForTest/verbose.
	switch prefix {
	case "[mcu-ota]":
		return event == "heartbeat_start" || event == "heartbeat_stop"
	case "[serial-raw]", "[fabric-rx]", "[fabric-rpc]", "[fabric-handshake]", "[fabric-xfer]", "[updater-stream]":
		return strings.HasSuffix(event, "_error") ||
			strings.Contains(event, "reject") ||
			strings.Contains(event, "abort")
	default:
		return false
	}
}

func allowFabricXferEvent(event string, fields []Field) bool {
	if strings.HasPrefix(event, "begin_") {
		return true
	}
	switch event {
	case "abort_local", "abort_tx", "done_tx", "ready_tx", "malformed_retry", "transfer_mem_sample":
		return true
	case "need_tx":
		next := fieldValue(fields, "next")
		return next == "0" ||
			fieldValue(fields, "ok") == "false" ||
			fieldValue(fields, "duplicate") == "true" ||
			fieldValue(fields, "retry") == "true" ||
			fieldValue(fields, "skipped") != ""
	case "chunk_decode_done", "chunk_digest_done":
		return fieldValue(fields, "ok") == "false"
	case "chunk_stale_offset", "chunk_future_offset", "chunk_size_overflow",
		"sink_write_error", "chunk_write_error":
		return true
	default:
		return strings.HasSuffix(event, "_error") ||
			strings.Contains(event, "reject") ||
			strings.Contains(event, "abort")
	}
}

func allowUpdaterStreamEvent(event string) bool {
	if strings.HasPrefix(event, "prepare_") {
		return true
	}
	switch event {
	case "begin_entry", "lease_ok", "begin_exit",
		"flash_erase_start", "flash_erase_done", "flash_erase_error",
		"flash_program_error", "program_page_error":
		return true
	default:
		return strings.HasSuffix(event, "_error") ||
			strings.Contains(event, "reject")
	}
}

func fieldValue(fields []Field, key string) string {
	for _, f := range fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
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

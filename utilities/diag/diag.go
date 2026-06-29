package diag

import (
	"sync/atomic"

	"devicecode-go/x/shmring"
	"devicecode-go/x/strconvx"
)

// Package diag provides a tiny UART1 diagnostic facade for code paths that are
// too far from the Reactor to use its local Logger directly.
//
// It deliberately has the same shape as TinyGo's print/println calls, but it
// does not use TinyGo's standard serial path. The Reactor attaches the UART1
// serial_raw TX ring by calling SetUART1 when the uart1 log session opens.
//
// There is no goroutine, channel, queue, or blocking path here. Writes are
// best-effort and lossy, matching the existing Logger mirror philosophy.

var (
	target *shmring.Ring
	busy   uint32

	droppedBusy  uint32
	droppedFull  uint32
	droppedBytes uint32
)

var crlf = [...]byte{'\r', '\n'}

func SetUART1(r *shmring.Ring) { target = r }
func Enabled() bool            { return target != nil }
func DroppedBusy() uint32      { return atomic.LoadUint32(&droppedBusy) }
func DroppedFull() uint32      { return atomic.LoadUint32(&droppedFull) }
func DroppedBytes() uint32     { return atomic.LoadUint32(&droppedBytes) }

func Print(parts ...any) {
	if !beginWrite() {
		return
	}
	defer endWrite()
	for i := range parts {
		writePart(parts[i])
	}
}

func Println(parts ...any) {
	if !beginWrite() {
		return
	}
	defer endWrite()
	for i := range parts {
		if i > 0 {
			writeString(" ")
		}
		writePart(parts[i])
	}
	writeBytes(crlf[:])
}

func Write(p []byte) {
	if len(p) == 0 || !beginWrite() {
		return
	}
	defer endWrite()
	writeBytesNormalised(p)
}

func WriteLine(p []byte) {
	if !beginWrite() {
		return
	}
	defer endWrite()
	writeBytesNormalised(p)
	writeBytes(crlf[:])
}

func beginWrite() bool {
	if target == nil {
		return false
	}
	if !atomic.CompareAndSwapUint32(&busy, 0, 1) {
		atomic.AddUint32(&droppedBusy, 1)
		return false
	}
	if target == nil {
		atomic.StoreUint32(&busy, 0)
		return false
	}
	return true
}

func endWrite() { atomic.StoreUint32(&busy, 0) }

func writePart(v any) {
	switch x := v.(type) {
	case string:
		writeString(x)
	case []byte:
		writeBytesNormalised(x)
	case int:
		writeString(strconvx.Itoa(x))
	case int32:
		writeString(strconvx.Itoa(int(x)))
	case int64:
		writeString(strconvx.Itoa64(x))
	case uint:
		writeString(strconvx.Itoa(int(x)))
	case uint32:
		writeString(strconvx.Itoa(int(x)))
	case uint64:
		writeString(strconvx.Itoa64(int64(x)))
	case bool:
		if x {
			writeString("true")
		} else {
			writeString("false")
		}
	default:
		writeString("?")
	}
}

func writeString(s string) {
	if s == "" {
		return
	}
	writeBytesNormalised([]byte(s))
}

func writeBytesNormalised(p []byte) {
	start := 0
	for i, b := range p {
		if b != '\n' {
			continue
		}
		if i > start {
			writeBytes(p[start:i])
		}
		writeBytes(crlf[:])
		start = i + 1
	}
	if start < len(p) {
		writeBytes(p[start:])
	}
}

func writeBytes(p []byte) {
	r := target
	if r == nil || len(p) == 0 {
		return
	}
	n := r.TryWriteFrom(p)
	if n < len(p) {
		atomic.AddUint32(&droppedFull, 1)
		atomic.AddUint32(&droppedBytes, uint32(len(p)-n))
	}
}

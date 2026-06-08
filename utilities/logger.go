package utilities

import (
	"time"

	"devicecode-go/x/shmring"
	"devicecode-go/x/strconvx"
)

// -----------------------------------------------------------------------------
// Logger (mirrors to USB console and optionally uart1). No heap churn.
// -----------------------------------------------------------------------------

type Logger struct {
	target            *shmring.Ring
	t0                time.Time
	LineStart         bool
	droppedUART1Bytes int // mirror dropped bytes
}

var nl = [...]byte{'\n'}

func (l *Logger) SetStart(t time.Time)     { l.t0, l.LineStart = t, true }
func (l *Logger) SetUART1(r *shmring.Ring) { l.target = r }

func (l *Logger) writeString(s string) {
	l.writePrefixIfLineStart()
	if s != "" {
		print(s)
		l.logWrite([]byte(s))
	}
}
func (l *Logger) writeBytes(b []byte) {
	if len(b) == 0 {
		return
	}
	l.writePrefixIfLineStart()
	print(string(b))
	l.logWrite(b)
}
func (l *Logger) writePrefixIfLineStart() {
	if !l.LineStart {
		return
	}
	l.LineStart = false
	if l.t0.IsZero() {
		l.t0 = time.Now()
	}
	el := time.Since(l.t0)
	secs := int(el / time.Second)
	ms := int((el % time.Second) / time.Millisecond) // 0..999

	// Console (no allocations)
	print(strconvx.Itoa(secs))
	print(".")
	if ms < 100 {
		print("0")
	}
	if ms < 10 {
		print("0")
	}
	print(strconvx.Itoa(ms))
	print(" ")

	// UART1: build once, single write
	if l.target != nil {
		var buf [20]byte
		n := 0
		n += writeDec(buf[:], n, secs)
		buf[n] = '.'
		n++
		n += writeDecPad3(buf[:], n, ms)
		buf[n] = ' '
		n++
		l.logWrite(buf[:n])
	}
}
func writeDecPad3(dst []byte, off int, v int) int {
	if v < 0 {
		v = 0
	} else if v > 999 {
		v = 999
	}
	dst[off+0] = byte('0' + (v/100)%10)
	dst[off+1] = byte('0' + (v/10)%10)
	dst[off+2] = byte('0' + v%10)
	return 3
}
func writeDec(dst []byte, off int, v int) int {
	if v == 0 {
		dst[off] = '0'
		return 1
	}
	var tmp [10]byte
	j := 0
	for v > 0 {
		tmp[j] = byte('0' + v%10)
		v /= 10
		j++
	}
	i := off
	for k := j - 1; k >= 0; k-- {
		dst[i] = tmp[k]
		i++
	}
	return i - off
}
func (l *Logger) writePart(v any) {
	switch x := v.(type) {
	case string:
		l.writeString(x)
	case []byte:
		l.writeBytes(x)
	case int:
		l.writeString(strconvx.Itoa(x))
	case int32:
		l.writeString(strconvx.Itoa(int(x)))
	case int64:
		l.writeString(strconvx.Itoa64(x))
	case uint:
		l.writeString(strconvx.Itoa(int(x)))
	case uint32:
		l.writeString(strconvx.Itoa(int(x)))
	case uint64:
		l.writeString(strconvx.Itoa64(int64(x)))
	case bool:
		if x {
			l.writeString("true")
		} else {
			l.writeString("false")
		}
	default:
		l.writeString("?")
	}
}
func (l *Logger) Print(parts ...any) {
	for i := range parts {
		l.writePart(parts[i])
	}
}
func (l *Logger) newline() {
	print("\n")
	l.logWrite(nl[:])
	l.LineStart = true
}
func (l *Logger) Println(parts ...any) { l.Print(parts...); l.newline() }

func (l *Logger) Deci(label string, deci int) {
	l.writePrefixIfLineStart()
	if deci < 0 {
		l.writeString(label)
		l.writeString("-")
		deci = -deci
	} else {
		l.writeString(label)
	}
	whole := deci / 10
	frac := deci % 10
	l.Println(strconvx.Itoa(whole), ".", strconvx.Itoa(frac))
}
func (l *Logger) Hundredths(label string, hx100 int) {
	l.writePrefixIfLineStart()
	if hx100 < 0 {
		hx100 = 0
	}
	whole := hx100 / 100
	frac := hx100 % 100
	if frac < 10 {
		l.Println(label, strconvx.Itoa(whole), ".0", strconvx.Itoa(frac))
	} else {
		l.Println(label, strconvx.Itoa(whole), ".", strconvx.Itoa(frac))
	}
}

// uart1 (logger mirror) — returns bytes written; tracks dropped bytes on partial writes.
func (l *Logger) logWrite(b []byte) int {
	if l == nil || l.target == nil || len(b) == 0 {
		return 0
	}
	n := l.target.TryWriteFrom(b)
	if n < len(b) {
		l.droppedUART1Bytes += (len(b) - n)
		// Avoid recursion; print to console directly.
		if l.droppedUART1Bytes == (len(b)-n) || (l.droppedUART1Bytes%1024) == 0 {
			print("[uart1] dropped bytes = ")
			print(strconvx.Itoa(l.droppedUART1Bytes))
			print("\n")
		}
	}
	return n
}

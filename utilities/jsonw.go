package utilities

import "devicecode-go/x/strconvx"

// -----------------------------------------------------------------------------
// Minimal streaming JSON writer for shmring (no buffers/allocs)
// -----------------------------------------------------------------------------

type JSONWriter struct {
	Write func([]byte) int
	first bool
}

func (w *JSONWriter) Begin() {
	w.first = true
	if w.Write != nil {
		w.Write([]byte("{"))
	}
}
func (w *JSONWriter) End() {
	if w.Write != nil {
		w.Write([]byte("}\n"))
	}
}
func (w *JSONWriter) Comma() {
	if w.Write == nil {
		return
	}
	if !w.first {
		w.Write([]byte(","))
	} else {
		w.first = false
	}
}
func (w *JSONWriter) Key(k string) {
	if w.Write == nil {
		return
	}
	w.Write([]byte(`"`))
	w.Write([]byte(k))
	w.Write([]byte(`":`))
}
func (w *JSONWriter) KvInt(k string, v int) {
	w.Comma()
	w.Key(k)
	if w.Write != nil {
		w.Write([]byte(strconvx.Itoa(v)))
	}
}
func (w *JSONWriter) KvStr(k, s string) {
	w.Comma()
	w.Key(k)
	if w.Write == nil {
		return
	}
	w.Write([]byte(`"`))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '"':
			w.Write([]byte{'\\', c})
		case '\b':
			w.Write([]byte{'\\', 'b'})
		case '\f':
			w.Write([]byte{'\\', 'f'})
		case '\n':
			w.Write([]byte{'\\', 'n'})
		case '\r':
			w.Write([]byte{'\\', 'r'})
		case '\t':
			w.Write([]byte{'\\', 't'})
		default:
			if c < 0x20 {
				var buf [6]byte
				buf[0], buf[1], buf[2], buf[3] = '\\', 'u', '0', '0'
				const hex = "0123456789abcdef"
				buf[4] = hex[c>>4]
				buf[5] = hex[c&0xF]
				w.Write(buf[:])
			} else {
				w.Write([]byte{c})
			}
		}
	}
	w.Write([]byte(`"`))
}
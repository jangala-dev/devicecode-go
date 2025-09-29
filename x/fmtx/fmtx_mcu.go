//go:build rp2040

package fmtx

import (
	"io"
	"unicode/utf8"

	"devicecode-go/x/strconvx"
)

// DefaultOutput is used by Print/Printf on MCU builds.
// Set this from your platform bootstrap (e.g. a UART writer).
var DefaultOutput io.Writer = discard{}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// --- Public API (signatures match fmt) ---

func Sprintf(format string, a ...any) string {
	var b builder
	b.format(format, a...)
	return string(b.buf)
}

func Printf(format string, a ...any) (int, error) {
	s := Sprintf(format, a...)
	return Fprint(DefaultOutput, s)
}

func Fprintf(w io.Writer, format string, a ...any) (int, error) {
	s := Sprintf(format, a...)
	return Fprint(w, s)
}

func Errorf(format string, a ...any) error {
	return &stringError{Sprintf(format, a...)}
}

func Sprint(a ...any) string {
	var b builder
	for i, v := range a {
		if i > 0 {
			b.byte(' ')
		}
		b.any(v, 'v')
	}
	return string(b.buf)
}

func Fprint(w io.Writer, a ...any) (int, error) {
	s := Sprint(a...)
	return w.Write([]byte(s))
}

func Print(a ...any) (int, error) { return Fprint(DefaultOutput, a...) }

// --- Internals: tiny formatter subset ---
// Supports: %s %q %d %x %X %v %t %% and width/precision for %s/%d (basic).
// No flags (+, space, #) beyond hex case; keep MCU cost low.

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

type builder struct{ buf []byte }

func (b *builder) byte(c byte)    { b.buf = append(b.buf, c) }
func (b *builder) bytes(p []byte) { b.buf = append(b.buf, p...) }
func (b *builder) str(s string)   { b.bytes([]byte(s)) }

func (b *builder) any(v any, verb rune) {
	switch x := v.(type) {
	case string:
		if verb == 'q' {
			b.quoteString(x)
		} else {
			b.str(x)
		}
	case []byte:
		if verb == 'q' {
			b.quoteString(string(x))
		} else {
			b.bytes(x)
		}
	case int:
		b.str(strconvx.FormatInt(int64(x), 10))
	case int8:
		b.str(strconvx.FormatInt(int64(x), 10))
	case int16:
		b.str(strconvx.FormatInt(int64(x), 10))
	case int32: // covers rune
		b.str(strconvx.FormatInt(int64(x), 10))
	case int64:
		b.str(strconvx.FormatInt(x, 10))
	case uint:
		b.str(strconvx.FormatUint(uint64(x), 10))
	case uint8: // covers byte
		b.str(strconvx.FormatUint(uint64(x), 10))
	case uint16:
		b.str(strconvx.FormatUint(uint64(x), 10))
	case uint32:
		b.str(strconvx.FormatUint(uint64(x), 10))
	case uint64:
		b.str(strconvx.FormatUint(x, 10))
	case bool:
		if x {
			b.str("true")
		} else {
			b.str("false")
		}
	case float32:
		b.str(strconvx.FormatFloat(float64(x), 'f', 6, 32))
	case float64:
		b.str(strconvx.FormatFloat(x, 'f', 6, 64))
	default:
		b.str("<unk>")
	}
}

func toU64(v any) uint64 {
	switch t := v.(type) {
	case uint:
		return uint64(t)
	case uint8:
		return uint64(t)
	case uint16:
		return uint64(t)
	case uint32:
		return uint64(t)
	default:
		return 0
	}
}

func (b *builder) format(format string, args ...any) {
	ai := 0
	for i := 0; i < len(format); {
		if format[i] != '%' {
			b.byte(format[i])
			i++
			continue
		}
		// "%%"
		if i+1 < len(format) && format[i+1] == '%' {
			b.byte('%')
			i += 2
			continue
		}
		i++
		// parse minimal width/precision: %<w>.<p><verb>
		width, prec, hasPrec := 0, 0, false
		i = parseNum(format, i, &width)
		if i < len(format) && format[i] == '.' {
			i++
			hasPrec = true
			i = parseNum(format, i, &prec)
		}
		if i >= len(format) || ai >= len(args) {
			return
		}
		verb := rune(format[i])
		arg := args[ai]
		ai++
		i++

		switch verb {
		case 's', 'q':
			var s string
			switch v := arg.(type) {
			case string:
				s = v
			case []byte:
				s = string(v)
			default:
				// fall back to %v
				b.any(arg, 'v')
				continue
			}
			if verb == 'q' {
				s = quote(s)
			}
			if hasPrec && prec < len(s) {
				s = s[:prec]
			}
			if pad := width - utf8.RuneCountInString(s); pad > 0 {
				for j := 0; j < pad; j++ {
					b.byte(' ')
				}
			}
			b.str(s)
		case 'd':
			b.str(strconvx.FormatInt(toI64(arg), 10))
		case 'x', 'X':
			h := strconvx.FormatUint(uint64(toI64(arg)), 16)
			if verb == 'X' {
				// upper-case hex
				for i := 0; i < len(h); i++ {
					c := h[i]
					if 'a' <= c && c <= 'f' {
						hb := []byte(h)
						for j := i; j < len(hb); j++ {
							if 'a' <= hb[j] && hb[j] <= 'f' {
								hb[j] -= ('a' - 'A')
							}
						}
						h = string(hb)
						break
					}
				}
			}
			b.str(h)
		case 't':
			if v, ok := arg.(bool); ok {
				if v {
					b.str("true")
				} else {
					b.str("false")
				}
			} else {
				b.str("false")
			}
		case 'v':
			b.any(arg, 'v')
		default:
			// Unknown verb: write it literally to aid debugging.
			b.byte('%')
			b.byte(byte(verb))
		}
	}
}

func toI64(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int8:
		return int64(t)
	case int16:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case uint, uint8, uint16, uint32, uint64:
		return int64(toU64(t))
	default:
		return 0
	}
}

func parseNum(s string, i int, out *int) int {
	n := 0
	start := i
	for i < len(s) && '0' <= s[i] && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i > start {
		*out = n
	}
	return i
}

func (b *builder) quoteString(s string) { b.str(quote(s)) }

func quote(s string) string {
	// Minimal %q implementation: escape backslash and quotes; keep others as-is.
	var out []byte
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '"':
			out = append(out, '\\', s[i])
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, s[i])
		}
	}
	out = append(out, '"')
	return string(out)
}

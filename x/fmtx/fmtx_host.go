//go:build !(rp2040 || rp2350)

package fmtx

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// DefaultOutput matches the MCU API surface so tests and callers can redirect
// Print/Printf without depending on the host's process stdout directly.
var DefaultOutput io.Writer = os.Stdout

func Sprintf(format string, a ...any) string                    { return fmt.Sprintf(format, a...) }
func Printf(format string, a ...any) (int, error)               { return Fprintf(DefaultOutput, format, a...) }
func Fprintf(w io.Writer, format string, a ...any) (int, error) { return fmt.Fprintf(w, format, a...) }
func Errorf(format string, a ...any) error                      { return fmt.Errorf(format, a...) }

// Keep host behavior aligned with the MCU formatter, which always separates
// Sprint/Fprint operands with spaces.
func Sprint(a ...any) string {
	var b strings.Builder
	for i, v := range a {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(fmt.Sprint(v))
	}
	return b.String()
}
func Fprint(w io.Writer, a ...any) (int, error) { return io.WriteString(w, Sprint(a...)) }
func Print(a ...any) (int, error)               { return Fprint(DefaultOutput, a...) }

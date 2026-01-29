//go:build !(rp2040 || rp2350)

package fmtx

import (
	"fmt"
	"io"
)

func Sprintf(format string, a ...any) string                    { return fmt.Sprintf(format, a...) }
func Printf(format string, a ...any) (int, error)               { return fmt.Printf(format, a...) }
func Fprintf(w io.Writer, format string, a ...any) (int, error) { return fmt.Fprintf(w, format, a...) }
func Errorf(format string, a ...any) error                      { return fmt.Errorf(format, a...) }
func Sprint(a ...any) string                                    { return fmt.Sprint(a...) }
func Fprint(w io.Writer, a ...any) (int, error)                 { return fmt.Fprint(w, a...) }
func Print(a ...any) (int, error)                               { return fmt.Print(a...) }

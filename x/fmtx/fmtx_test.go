package fmtx

import (
	"bytes"
	"errors"
	"testing"
)

func TestSprintfVerbs(t *testing.T) {
	type C struct {
		fmt  string
		args []any
		want string
	}
	for _, c := range []C{
		{"hello %s", []any{"world"}, "hello world"},
		{"num %d hex %x HEX %X", []any{255, 255, 255}, "num 255 hex ff HEX FF"},
		{"bool %t %t", []any{true, false}, "bool true false"},
		{"literal %%", nil, "literal %"},
		{"q=%q", []any{"a\"b\\c"}, `q="a\"b\\c"`},
		{"v=%v", []any{123}, "v=123"},
		{"trim: %.3s", []any{"abcdef"}, "trim: abc"},
	} {
		got := Sprintf(c.fmt, c.args...)
		if got != c.want {
			t.Fatalf("Sprintf(%q, ...) = %q, want %q", c.fmt, got, c.want)
		}
	}
}

func TestSprintAndFprint(t *testing.T) {
	var buf bytes.Buffer
	DefaultOutput = &buf

	// Sprint joins with spaces
	if got, want := Sprint("a", 1, true), "a 1 true"; got != want {
		t.Fatalf("Sprint = %q, want %q", got, want)
	}

	// Print writes to DefaultOutput
	buf.Reset()
	n, err := Print("x", 2)
	if err != nil {
		t.Fatalf("Print error: %v", err)
	}
	if n <= 0 {
		t.Fatalf("Print wrote %d bytes, want >0", n)
	}
	if got, want := buf.String(), "x 2"; got != want {
		t.Fatalf("Print wrote %q, want %q", got, want)
	}

	// Printf formatting
	buf.Reset()
	_, _ = Printf("v=%d", 7)
	if got, want := buf.String(), "v=7"; got != want {
		t.Fatalf("Printf wrote %q, want %q", got, want)
	}
}

func TestFprintf(t *testing.T) {
	var buf bytes.Buffer
	_, err := Fprintf(&buf, "hi %s", "there")
	if err != nil {
		t.Fatalf("Fprintf error: %v", err)
	}
	if got, want := buf.String(), "hi there"; got != want {
		t.Fatalf("Fprintf wrote %q, want %q", got, want)
	}
}

func TestErrorf(t *testing.T) {
	err := Errorf("bad %s: %d", "thing", 3)
	if err == nil {
		t.Fatalf("Errorf returned nil")
	}
	if err.Error() != "bad thing: 3" {
		t.Fatalf("Errorf string = %q, want %q", err.Error(), "bad thing: 3")
	}
	// Ensure it satisfies error semantics
	if !errors.Is(err, err) {
		t.Fatalf("errors.Is should be true on itself")
	}
}

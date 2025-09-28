package strconvx

import "testing"

func TestItoaAtoi(t *testing.T) {
	cases := []int{0, 1, -1, 42, -99999}
	for _, v := range cases {
		s := Itoa(v)
		got, err := Atoi(s)
		if err != nil {
			t.Fatalf("Atoi(%q) error: %v", s, err)
		}
		if got != v {
			t.Fatalf("Itoa/Atoi round trip: want %d, got %d", v, got)
		}
	}
}

func TestFormatIntUintBases(t *testing.T) {
	type C struct {
		u    uint64
		base int
		want string
	}
	for _, c := range []C{
		{0, 2, "0"},
		{5, 2, "101"},
		{255, 16, "ff"},
		{255, 10, "255"},
		{35, 36, "z"},
	} {
		if got := FormatUint(c.u, c.base); got != c.want {
			t.Fatalf("FormatUint(%d,%d) = %q, want %q", c.u, c.base, got, c.want)
		}
	}
	if got := FormatInt(-15, 10); got != "-15" {
		t.Fatalf("FormatInt(-15,10) = %q, want -15", got)
	}
}

func TestParseUintBaseAutoAndExplicit(t *testing.T) {
	type C struct {
		s    string
		base int
		want uint64
	}
	for _, c := range []C{
		{"0", 0, 0},
		{"101", 2, 5},
		{"0b101", 0, 5},
		{"075", 0, 75}, // note: our detectBase treats 0o/0O, not bare 0-prefix; this remains base-10
		{"0o77", 0, 63},
		{"0O77", 0, 63},
		{"0xff", 0, 255},
		{"0Xff", 0, 255},
		{"FF", 16, 255},
	} {
		got, err := ParseUint(c.s, c.base, 64)
		if err != nil {
			t.Fatalf("ParseUint(%q,%d) error: %v", c.s, c.base, err)
		}
		if got != c.want {
			t.Fatalf("ParseUint(%q,%d) = %d, want %d", c.s, c.base, got, c.want)
		}
	}
}

func TestParseUintErrors(t *testing.T) {
	for _, s := range []string{"", "g", "0x", "2", "0b102"} {
		if _, err := ParseUint(s, 2, 64); err == nil {
			t.Fatalf("ParseUint(%q,2) expected error", s)
		}
	}
}

func TestParseIntSignsAndLimits(t *testing.T) {
	type C struct {
		s    string
		base int
		want int64
	}
	for _, c := range []C{
		{"+10", 10, 10},
		{"-10", 10, -10},
		{"0b11", 0, 3},
		{"-0x0f", 0, -15},
	} {
		got, err := ParseInt(c.s, c.base, 64)
		if err != nil {
			t.Fatalf("ParseInt(%q,%d) error: %v", c.s, c.base, err)
		}
		if got != c.want {
			t.Fatalf("ParseInt(%q,%d) = %d, want %d", c.s, c.base, got, c.want)
		}
	}
	// Overflow checks are conservative in MCU implementation; ensure an obviously large value errors for signed.
	if _, err := ParseInt("18446744073709551615", 10, 64); err == nil {
		t.Fatalf("ParseInt(too big) expected error")
	}
}

func TestFormatParseFloatBasic(t *testing.T) {
	type C struct {
		in   float64
		prec int
		want string
	}
	for _, c := range []C{
		{0, 0, "0"},
		{12.3, 1, "12.3"},
		{12.345, 2, "12.35"}, // rounding
		{-1.25, 2, "-1.25"},
	} {
		got := FormatFloat(c.in, 'f', c.prec, 64)
		if got != c.want {
			t.Fatalf("FormatFloat(%v,'f',%d) = %q, want %q", c.in, c.prec, got, c.want)
		}
		// Parse back (when there is a fractional part, precision must match).
		v, err := ParseFloat(got, 64)
		if err != nil {
			t.Fatalf("ParseFloat(%q) error: %v", got, err)
		}
		// Accept small error due to simplified formatting/parsing (should be exact here).
		if FormatFloat(v, 'f', c.prec, 64) != c.want {
			t.Fatalf("round-trip mismatch for %q", c.want)
		}
	}

	// Error path
	if _, err := ParseFloat("12.3.4", 64); err == nil {
		t.Fatalf("ParseFloat invalid expected error")
	}
}

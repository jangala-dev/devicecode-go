package util

import (
	"testing"
	"time"
)

func TestDecodeJSON(t *testing.T) {
	type P struct {
		A int    `json:"a"`
		B string `json:"b"`
	}

	for name, in := range map[string]any{
		"bytes":  []byte(`{"a":1,"b":"x"}`),
		"string": `{"a":1,"b":"x"}`,
		"map":    map[string]any{"a": 1, "b": "x"},
	} {
		var p P
		if err := DecodeJSON(in, &p); err != nil {
			t.Fatalf("%s: decode failed: %v", name, err)
		}
		if p.A != 1 || p.B != "x" {
			t.Fatalf("%s: unexpected result: %+v", name, p)
		}
	}
}

func TestClampIntAndBoolToInt(t *testing.T) {
	if ClampInt(-5, 0, 10) != 0 {
		t.Fatal("clamp low failed")
	}
	if ClampInt(15, 0, 10) != 10 {
		t.Fatal("clamp high failed")
	}
	if ClampInt(7, 0, 10) != 7 {
		t.Fatal("clamp mid failed")
	}
	if BoolToInt(true) != 1 || BoolToInt(false) != 0 {
		t.Fatal("BoolToInt failed")
	}
}

func TestResetAndDrainTimer(t *testing.T) {
	tm := time.NewTimer(time.Hour)
	if !tm.Stop() {
		DrainTimer(tm)
	}
	// Reset to near-zero and ensure it fires quickly.
	ResetTimer(tm, 1*time.Millisecond)
	select {
	case <-tm.C:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("timer did not fire after ResetTimer")
	}
	// Negative reset clamps to zero and should fire immediately.
	ResetTimer(tm, -1)
	select {
	case <-tm.C:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("timer did not fire after negative ResetTimer")
	}
}

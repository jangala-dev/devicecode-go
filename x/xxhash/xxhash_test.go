package xxhash

import (
	"bytes"
	"testing"
)

// Reference vectors validated against
// devicecode-lua/src/shared/hash/xxhash32.lua at update-migration tip
// (commit 2c88090) using `print(M.digest_hex(input))` with seed 0.
var refVectors = []struct {
	name  string
	input string
	hex   string
}{
	{"empty", "", "02cc5d05"},
	{"a", "a", "550d7456"},
	{"abc", "abc", "32d153ff"},
	{"123456789", "123456789", "937bad67"},
}

func TestSumHex_KnownAnswer(t *testing.T) {
	for _, v := range refVectors {
		got := SumHex([]byte(v.input))
		if got != v.hex {
			t.Errorf("SumHex(%q): got %s, want %s", v.input, got, v.hex)
		}
	}
}

func TestSum32_KnownAnswer(t *testing.T) {
	// Sum32(_, 0) must agree with SumHex (which forces seed 0).
	for _, v := range refVectors {
		want := SumHex([]byte(v.input))
		got := hex8(Sum32([]byte(v.input), 0))
		if got != want {
			t.Errorf("Sum32(%q, 0): got %s, want %s", v.input, got, want)
		}
	}
}

func TestVerifyHex(t *testing.T) {
	for _, v := range refVectors {
		if !VerifyHex([]byte(v.input), v.hex) {
			t.Errorf("VerifyHex(%q, %s) returned false", v.input, v.hex)
		}
		if VerifyHex([]byte(v.input), "deadbeef") {
			t.Errorf("VerifyHex(%q, deadbeef) returned true", v.input)
		}
	}
}

func TestStreaming_ByteByByte(t *testing.T) {
	for _, v := range refVectors {
		h := New(0)
		for _, b := range []byte(v.input) {
			h.Write([]byte{b})
		}
		got := hex8(h.Sum32())
		if got != v.hex {
			t.Errorf("byte-stream %q: got %s, want %s", v.input, got, v.hex)
		}
	}
}

func TestStreaming_OddSplits(t *testing.T) {
	// A 32-byte input spans two 16-byte blocks, so splits at 1, 7, 15, 16,
	// 17, and 31 exercise mem-buffer top-up, exact block boundary, and tail
	// bytes.
	in := []byte("0123456789abcdef0123456789abcdef")
	want := SumHex(in)

	for _, split := range []int{0, 1, 7, 15, 16, 17, 31, 32} {
		h := New(0)
		h.Write(in[:split])
		h.Write(in[split:])
		got := hex8(h.Sum32())
		if got != want {
			t.Errorf("split=%d: got %s, want %s", split, got, want)
		}
	}
}

func TestStreaming_EmptyWritesNoOp(t *testing.T) {
	h := New(0)
	h.Write(nil)
	h.Write([]byte{})
	h.Write([]byte("abc"))
	h.Write([]byte{})
	if got := hex8(h.Sum32()); got != "32d153ff" {
		t.Errorf("with empty writes interleaved: got %s, want 32d153ff", got)
	}
}

func TestReset(t *testing.T) {
	h := New(0)
	h.Write([]byte("abc"))
	if hex8(h.Sum32()) != "32d153ff" {
		t.Fatalf("first sum mismatch")
	}
	h.Reset()
	h.Write([]byte("abc"))
	if hex8(h.Sum32()) != "32d153ff" {
		t.Fatalf("post-reset sum mismatch")
	}
}

func TestSeedNonZero(t *testing.T) {
	in := []byte("the quick brown fox jumps over the lazy dog")
	if Sum32(in, 0) == Sum32(in, 1) {
		t.Fatalf("seeds 0 and 1 produced same hash")
	}
	h := New(42)
	h.Write(in)
	if h.Sum32() != Sum32(in, 42) {
		t.Fatalf("streaming with seed=42 != one-shot")
	}
}

func TestSum32Idempotent(t *testing.T) {
	// Sum32 should not mutate state; calling it twice must give the same result.
	h := New(0)
	h.Write([]byte("abc"))
	a := h.Sum32()
	b := h.Sum32()
	if a != b {
		t.Errorf("Sum32 not idempotent: %x != %x", a, b)
	}
}

func TestSum32ContinuesAfter(t *testing.T) {
	// Calling Sum32, then Write, then Sum32 again must reflect the new bytes.
	h := New(0)
	h.Write([]byte("a"))
	h.Sum32()
	h.Write([]byte("bc"))
	got := hex8(h.Sum32())
	if got != "32d153ff" {
		t.Errorf("post-Sum32 continuation: got %s, want 32d153ff", got)
	}
}

func TestLargeBuffer(t *testing.T) {
	// Confirm one-shot and streaming agree on a buffer comfortably larger
	// than the 16-byte block size; this exercises the hot loop in Write.
	in := bytes.Repeat([]byte("0123456789abcdef"), 64) // 1024 bytes
	want := Sum32(in, 0)

	h := New(0)
	for i := 0; i < len(in); i += 7 {
		end := min(i+7, len(in))
		h.Write(in[i:end])
	}
	if got := h.Sum32(); got != want {
		t.Errorf("1024-byte streaming vs one-shot: got %x, want %x", got, want)
	}
}

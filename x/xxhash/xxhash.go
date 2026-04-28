// Package xxhash implements the xxHash32 algorithm — a fast, non-cryptographic
// 32-bit hash.
//
// This package mirrors devicecode-lua/src/shared/hash/xxhash32.lua at
// update-migration tip (commit 2c88090). It is used for fabric wire-protocol
// integrity (xfer_begin / xfer_commit checksum field) and for HAL artefact
// hashing. It is not a security primitive.
package xxhash

import "math/bits"

// xxHash32 round constants. These match the canonical xxHash32 spec and the
// Lua reference at src/shared/hash/xxhash32.lua.
const (
	prime32_1 uint32 = 0x9E3779B1
	prime32_2 uint32 = 0x85EBCA77
	prime32_3 uint32 = 0xC2B2AE3D
	prime32_4 uint32 = 0x27D4EB2F
	prime32_5 uint32 = 0x165667B1
)

// Hasher is a streaming xxHash32 state.
type Hasher struct {
	seed     uint32
	totalLen uint32
	v1, v2, v3, v4 uint32
	mem      [16]byte
	memN     uint8 // 0..15
	large    bool  // true once a 16-byte block has been absorbed
}

// New returns a streaming xxHash32 hasher seeded with seed.
func New(seed uint32) *Hasher {
	h := &Hasher{}
	h.reset(seed)
	return h
}

// Reset re-initialises the hasher with seed 0. To re-seed with a different
// value, allocate a new Hasher with New.
func (h *Hasher) Reset() { h.reset(0) }

func (h *Hasher) reset(seed uint32) {
	h.seed = seed
	h.totalLen = 0
	h.v1 = seed + prime32_1 + prime32_2
	h.v2 = seed + prime32_2
	h.v3 = seed
	h.v4 = seed - prime32_1
	h.memN = 0
	h.large = false
}

// Write absorbs p into the running hash. Always returns (len(p), nil).
func (h *Hasher) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	h.totalLen += uint32(n)

	// Top up the partial-block buffer if it has any bytes.
	if h.memN > 0 {
		need := 16 - int(h.memN)
		if n < need {
			copy(h.mem[h.memN:], p)
			h.memN += uint8(n)
			return n, nil
		}
		copy(h.mem[h.memN:], p[:need])
		h.absorbBlock(h.mem[:])
		p = p[need:]
		h.memN = 0
	}

	// Absorb aligned 16-byte blocks directly from p.
	for len(p) >= 16 {
		h.absorbBlock(p[:16])
		p = p[16:]
	}

	// Stash the trailing remainder (0..15 bytes).
	if len(p) > 0 {
		copy(h.mem[:], p)
		h.memN = uint8(len(p))
	}
	return n, nil
}

func (h *Hasher) absorbBlock(b []byte) {
	h.large = true
	h.v1 = round(h.v1, leU32(b[0:4]))
	h.v2 = round(h.v2, leU32(b[4:8]))
	h.v3 = round(h.v3, leU32(b[8:12]))
	h.v4 = round(h.v4, leU32(b[12:16]))
}

// Sum32 returns the xxHash32 of all bytes absorbed so far. It does not modify
// the hasher state; calling Write afterwards continues the hash.
func (h *Hasher) Sum32() uint32 {
	var x uint32
	if h.large {
		x = bits.RotateLeft32(h.v1, 1) +
			bits.RotateLeft32(h.v2, 7) +
			bits.RotateLeft32(h.v3, 12) +
			bits.RotateLeft32(h.v4, 18)
	} else {
		x = h.seed + prime32_5
	}

	x += h.totalLen

	rem := h.mem[:h.memN]
	for len(rem) >= 4 {
		x += leU32(rem) * prime32_3
		x = bits.RotateLeft32(x, 17) * prime32_4
		rem = rem[4:]
	}
	for _, b := range rem {
		x += uint32(b) * prime32_5
		x = bits.RotateLeft32(x, 11) * prime32_1
	}

	x ^= x >> 15
	x *= prime32_2
	x ^= x >> 13
	x *= prime32_3
	x ^= x >> 16
	return x
}

// Sum32 computes the xxHash32 of p with the given seed in one pass.
func Sum32(p []byte, seed uint32) uint32 {
	var h Hasher
	h.reset(seed)
	_, _ = h.Write(p)
	return h.Sum32()
}

// SumHex returns the xxHash32 of p (seed 0) as 8 lower-case hex characters,
// matching the wire format used by the Lua reference's M.digest_hex.
func SumHex(p []byte) string { return hex8(Sum32(p, 0)) }

// VerifyHex compares SumHex(p) to expected for case-sensitive equality.
func VerifyHex(p []byte, expected string) bool { return SumHex(p) == expected }

func round(acc, lane uint32) uint32 {
	acc += lane * prime32_2
	acc = bits.RotateLeft32(acc, 13)
	acc *= prime32_1
	return acc
}

func leU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

const hexdigits = "0123456789abcdef"

func hex8(v uint32) string {
	var buf [8]byte
	for i := 7; i >= 0; i-- {
		buf[i] = hexdigits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

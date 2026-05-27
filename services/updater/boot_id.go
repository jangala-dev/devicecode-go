package updater

import (
	"encoding/hex"
	"runtime"
	"sync/atomic"
	"time"
)

// boot_id contract per master plan R3 / docs/firmware-alignment-update.md §W6:
//   - Opaque 16-character lower-hex marker that must change on every
//     successful boot.
//   - Generated from 8 bytes of crypto/rand AFTER HAL init succeeds and
//     BEFORE fabric opens, so it's available to the first software-fact
//     publish on hello_ack.
//   - Held in RAM only. Not persisted to flash. Not added to the
//     abupdate metadata block (the regression guard test in
//     fabric-update tests checks that abupdate metadata never grows a
//     boot_id field).
//
// The fallback path on rand failure is documented inline; this branch
// drops to a process-startup counter rather than panicking, with a
// clear log so the failure-mode test suite (master R3) can assert it.

var (
	cachedBootID atomic.Pointer[string]
	fallbackTick uint64
)

// GenerateBootID populates the cached value. Call exactly once during
// boot — main.go invokes it between HAL ready and fabric.Run. Subsequent
// calls return the existing value (idempotent so reactor reinit doesn't
// regenerate).
func GenerateBootID() string {
	if existing := cachedBootID.Load(); existing != nil {
		return *existing
	}
	id := generate()
	if cachedBootID.CompareAndSwap(nil, &id) {
		return id
	}
	// Lost the race; return whatever the winner stored.
	return *cachedBootID.Load()
}

// BootID returns the cached value generated at boot. Returns "" if
// GenerateBootID has not yet been called — which would indicate a
// boot-order bug, since the spec says it must run before fabric opens.
func BootID() string {
	if existing := cachedBootID.Load(); existing != nil {
		return *existing
	}
	return ""
}

func generate() string {
	var buf [8]byte
	if tryCryptoRand(buf[:]) {
		return hex.EncodeToString(buf[:])
	}
	// Fallback: triggered only when crypto/rand is unavailable or returns
	// all-zero. This is best-effort per-boot jitter, not contract-grade entropy.
	// The log line below is the failure-mode signal for tests and diagnostics.
	tick := atomic.AddUint64(&fallbackTick, 1)
	println("[updater] boot_id fallback engaged tick=", itoaU64(tick))

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// Best-effort fallback when crypto/rand is unavailable. Mixes:
	//   - monotonic clock at generation time (UnixNano), which varies
	//     with HAL init duration across cold boots;
	//   - runtime.MemStats Alloc / Mallocs / HeapInuse / Frees, which
	//     vary with allocation timing inside HAL bringup;
	//   - the per-call counter so multiple GenerateBootID calls within
	//     one process boot don't collide.
	// Followed by a 3-stage shift mix so every output byte depends on
	// every input bit.
	//
	// NOT contract-grade: the mix is non-cryptographic and depends on runtime
	// jitter rather than a hardware entropy source. If crypto/rand is broken on
	// the target, use the RP2350 hardware RNG or a persisted boot counter.
	mix := tick
	mix ^= uint64(time.Now().UnixNano())
	mix ^= ms.Alloc
	mix ^= uint64(ms.Mallocs)
	mix ^= uint64(ms.HeapInuse)
	mix ^= uint64(ms.Frees) << 32
	mix ^= mix >> 11
	mix ^= mix << 17
	mix ^= mix >> 5
	for i := 7; i >= 0; i-- {
		buf[i] = byte(mix & 0xff)
		mix >>= 8
	}
	return hex.EncodeToString(buf[:])
}

// tryCryptoRand is split per build:
//   - host (!tinygo) — boot_id_host.go reads from crypto/rand
//   - tinygo (RP2350 et al.) — boot_id_tinygo.go skips crypto/rand
//     entirely and returns false, so the firmware always falls
//     through to the deterministic mix below. TinyGo on RP2350
//     panics with "no rng" inside crypto/rand.Read, and pulling in
//     defer/recover to catch it grew the binary by ~110 KB. Until
//     TinyGo wires the RP2350 hardware-RNG (rosc) into its
//     crypto/rand backend or we route a HAL-supplied RNG into
//     services/updater, the safe-by-default path on the firmware
//     is to never call crypto/rand.

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func itoaU64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[pos:])
}

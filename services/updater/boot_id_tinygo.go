//go:build tinygo

package updater

// tryCryptoRand on TinyGo always returns false so generate() falls
// through to the deterministic mix.
//
// Why we don't call crypto/rand.Read here: on RP2350 (and several
// other TinyGo targets), the runtime has no hardware-RNG seam wired
// in, and crypto/rand.Read PANICS with "no rng" rather than
// returning an error. Recovering from that panic in Go is possible
// but pulls TinyGo's panic-handling runtime into the binary,
// inflating code size by ~110 KB. The deterministic mix is poor
// entropy but at least it boots — and the
// `[updater] boot_id fallback engaged` log line is the canonical
// signal for the failure-mode hardware test suite.
//
// When TinyGo grows an RP2350 RNG backend or we route a HAL-
// supplied RNG into services/updater, drop this stub and let the
// host-side implementation (boot_id_host.go) be the single source.
func tryCryptoRand(buf []byte) bool {
	_ = buf
	return false
}

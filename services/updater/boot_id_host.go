//go:build !tinygo

package updater

import "crypto/rand"

// tryCryptoRand on host builds reads 8 bytes from crypto/rand. Tests
// assert randomness across simulated "boots" via this path.
func tryCryptoRand(buf []byte) bool {
	n, err := rand.Read(buf)
	if err != nil || n != len(buf) {
		return false
	}
	return !allZero(buf)
}

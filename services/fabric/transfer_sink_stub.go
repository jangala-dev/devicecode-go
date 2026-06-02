//go:build !(tinygo && rp2350)

// Host build (tests, dev tooling): same buffer-sink behaviour as the
// default RP2350 build. Lets unit tests exercise updater/main staging
// without firmware stubs in the way.

package fabric

func beginTransfer(meta transferMeta) (transferSink, error) {
	return newBufferSink(meta)
}

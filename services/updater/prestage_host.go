//go:build !tinygo || !rp2350

package updater

type streamedStage struct {
	Length        uint32
	PayloadSHA256 string
}

func consumeStreamedStage() (streamedStage, bool) {
	return streamedStage{}, false
}

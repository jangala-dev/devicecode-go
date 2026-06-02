//go:build !tinygo || !rp2350

package updater

import "errors"

type streamedStage struct {
	Version       string
	BuildID       string
	ImageID       string
	Length        uint32
	PayloadSHA256 string
}

func startStreamedStage(xferID string, generation uint64, size uint32) error {
	_, _, _ = xferID, generation, size
	return nil
}

func writeStreamedStage(xferID string, generation uint64, data []byte) error {
	_, _, _ = xferID, generation, data
	return errors.New("streamed_stage_not_supported")
}

func commitStreamedStage(xferID string, generation uint64) (streamedStage, error) {
	_, _ = xferID, generation
	return streamedStage{}, errors.New("streamed_stage_not_supported")
}

func abortStreamedStage() {}

func consumeStreamedStageResult() (streamedStage, bool) {
	return streamedStage{}, false
}

func discardStreamedStageResult() {
	clearABUpdateDiagHook()
}

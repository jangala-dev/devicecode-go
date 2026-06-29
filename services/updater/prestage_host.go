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

func startStreamedStage(size uint32) error {
	_ = size
	return nil
}

func writeStreamedStage(data []byte) error {
	_ = data
	return errors.New("streamed_stage_not_supported")
}

func commitStreamedStage() (streamedStage, error) {
	return streamedStage{}, errors.New("streamed_stage_not_supported")
}

func abortStreamedStage() {}

func consumeStreamedStageResult() (streamedStage, bool) {
	return streamedStage{}, false
}

func discardStreamedStageResult() {}

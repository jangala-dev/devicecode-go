//go:build tinygo && rp2350

package updater

import (
	"errors"

	"pico2-a-b/abupdate"
	"pico2-a-b/imagev1"
)

// streamedStage tracks a signed transfer that verified successfully while
// fabric streamed it. Only the signed payload bytes are written to the inactive
// A/B slot; the container header, manifest and signature are never staged.
type streamedStage struct {
	Version       string
	BuildID       string
	ImageID       string
	Length        uint32
	PayloadSHA256 string
}

var (
	streamedStageDesc streamedStage
	streamedStageOK   bool
	streamedVerifier  *imagev1.StreamVerifier
)

func startStreamedStage(size uint32) error {
	// A fresh prepare invalidates any prior stage, and retrying an update in
	// the same boot must not inherit abupdate's previous writing/complete
	// state. Recreate the updater before resolving the inactive slot.
	sharedUpdater = abupdate.Updater{}
	sharedUpdaterInit = false

	_ = size
	streamedStageDesc = streamedStage{}
	streamedStageOK = false
	streamedVerifier = imagev1.NewStreamVerifier(SignedImagePolicy(), func(payloadLen uint32) (imagev1.PayloadSink, error) {
		return newSlotSink(payloadLen)
	})
	return nil
}

func writeStreamedStage(data []byte) error {
	if len(data) == 0 {
		return errors.New("empty_chunk")
	}
	if streamedVerifier == nil {
		return errors.New("streamed_stage_not_started")
	}
	_, err := streamedVerifier.Write(data)
	return err
}

func commitStreamedStage() (streamedStage, error) {
	if streamedVerifier == nil {
		return streamedStage{}, errors.New("streamed_stage_not_started")
	}
	res, err := streamedVerifier.Commit()
	if err != nil {
		streamedVerifier = nil
		return streamedStage{}, err
	}
	streamedStageDesc = streamedStage{
		Version:       res.Version,
		BuildID:       res.BuildID,
		ImageID:       res.ImageID,
		Length:        res.PayloadLength,
		PayloadSHA256: res.PayloadSHA256,
	}
	streamedStageOK = true
	streamedVerifier = nil
	return streamedStageDesc, nil
}

func abortStreamedStage() {
	if streamedVerifier != nil {
		_ = streamedVerifier.Abort()
		streamedVerifier = nil
	}
	streamedStageDesc = streamedStage{}
	streamedStageOK = false
}

func consumeStreamedStageResult() (streamedStage, bool) {
	if !streamedStageOK {
		return streamedStage{}, false
	}
	out := streamedStageDesc
	streamedStageDesc = streamedStage{}
	streamedStageOK = false
	return out, true
}

func discardStreamedStageResult() {
	if streamedVerifier != nil {
		_ = streamedVerifier.Abort()
		streamedVerifier = nil
	}
	streamedStageDesc = streamedStage{}
	streamedStageOK = false
}

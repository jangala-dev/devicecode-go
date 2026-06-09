//go:build tinygo && rp2350 && !fabric_uart_hwtest

package updater

import (
	"errors"
	"time"

	"devicecode-go/services/otadiag"
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

func startStreamedStage(xferID string, generation uint64, size uint32) error {
	// A fresh prepare invalidates any prior stage, and retrying an update in
	// the same boot must not inherit abupdate's previous writing/complete
	// state. Recreate the updater before resolving the inactive slot.
	sharedUpdater = abupdate.Updater{}
	sharedUpdaterInit = false

	_ = size
	streamedStageDesc = streamedStage{}
	streamedStageOK = false
	streamedVerifier = imagev1.NewStreamVerifier(SignedImagePolicy(), func(payloadLen uint32) (imagev1.PayloadSink, error) {
		start := time.Now()
		otadiag.Event(
			"[updater-stream]", "slot_sink_create_start", xferID,
			otadiag.KV("generation", generation),
			otadiag.KV("payload_len", payloadLen),
		)
		sink, err := newSlotSink(payloadLen)
		if err != nil {
			otadiag.Event(
				"[updater-stream]", "slot_sink_create_error", xferID,
				otadiag.KV("generation", generation),
				otadiag.KV("err", err.Error()),
				otadiag.KV("dur_ms", int(time.Since(start)/time.Millisecond)),
			)
			return nil, err
		}
		otadiag.Event(
			"[updater-stream]", "slot_sink_create_done", xferID,
			otadiag.KV("generation", generation),
			otadiag.KV("dur_ms", int(time.Since(start)/time.Millisecond)),
		)
		return sink, nil
	})
	return nil
}

func writeStreamedStage(xferID string, generation uint64, data []byte) error {
	if len(data) == 0 {
		return errors.New("empty_chunk")
	}
	if streamedVerifier == nil {
		return errors.New("streamed_stage_not_started")
	}
	start := time.Now()
	otadiag.Event(
		"[updater-stream]", "stream_write_start", xferID,
		otadiag.KV("generation", generation),
		otadiag.KV("len", len(data)),
	)
	_, err := streamedVerifier.Write(data)
	if err != nil {
		otadiag.Event(
			"[updater-stream]", "stream_write_error", xferID,
			otadiag.KV("generation", generation),
			otadiag.KV("err", err.Error()),
			otadiag.KV("dur_ms", int(time.Since(start)/time.Millisecond)),
		)
		return err
	}
	otadiag.Event(
		"[updater-stream]", "stream_write_done", xferID,
		otadiag.KV("generation", generation),
		otadiag.KV("dur_ms", int(time.Since(start)/time.Millisecond)),
	)
	return err
}

func commitStreamedStage(svc *Service, xferID string, generation uint64) (streamedStage, error) {
	_, _, _ = svc, xferID, generation
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
	clearABUpdateDiagHook()
}

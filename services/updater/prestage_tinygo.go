//go:build tinygo && rp2350

package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"pico2-a-b/abupdate"
)

// streamedStage tracks a raw transfer that fabric has already streamed into
// the inactive A/B slot. It is the TinyGo bring-up path used before imagev1
// verification can stream directly from the transfer source.
type streamedStage struct {
	Length        uint32
	PayloadSHA256 string
}

var (
	streamedStageDesc streamedStage
	streamedStageOK   bool
	streamedStageHash = sha256.New()
	streamedStageLen  uint32
)

func startStreamedStage(size uint32) error {
	// A fresh prepare invalidates any prior stage, and retrying an update in
	// the same boot must not inherit abupdate's previous writing/complete
	// state. Recreate the updater before resolving the inactive slot.
	sharedUpdater = abupdate.Updater{}
	sharedUpdaterInit = false

	u, err := ensureUpdaterInited()
	if err != nil {
		return err
	}
	if rc := u.BeginUpdate(size); rc != 0 {
		return errFromRC("begin_update", rc)
	}
	streamedStageHash.Reset()
	streamedStageLen = 0
	streamedStageDesc = streamedStage{}
	streamedStageOK = false
	return nil
}

func writeStreamedStage(data []byte) error {
	if len(data) == 0 {
		return errors.New("empty_chunk")
	}
	u, err := ensureUpdaterInited()
	if err != nil {
		return err
	}
	if rc := u.WriteChunk(data); rc != 0 {
		return errFromRC("write_chunk", rc)
	}
	_, _ = streamedStageHash.Write(data)
	streamedStageLen += uint32(len(data))
	return nil
}

func commitStreamedStage() (streamedStage, error) {
	u, err := ensureUpdaterInited()
	if err != nil {
		return streamedStage{}, err
	}
	if rc := u.FlushFinal(); rc != 0 {
		return streamedStage{}, errFromRC("flush_final", rc)
	}
	streamedStageDesc = streamedStage{
		Length:        streamedStageLen,
		PayloadSHA256: hex.EncodeToString(streamedStageHash.Sum(nil)),
	}
	streamedStageOK = true
	if written := u.BytesWritten(); written != streamedStageDesc.Length {
		streamedStageDesc.Length = written
	}
	return streamedStageDesc, nil
}

func abortStreamedStage() {
	streamedStageDesc = streamedStage{}
	streamedStageOK = false
	streamedStageLen = 0
	streamedStageHash.Reset()
}

func consumeStreamedStageResult() (streamedStage, bool) {
	if !streamedStageOK {
		return streamedStage{}, false
	}
	out := streamedStageDesc
	streamedStageDesc = streamedStage{}
	streamedStageOK = false
	streamedStageLen = 0
	streamedStageHash.Reset()
	return out, true
}

func discardStreamedStageResult() {
	abortStreamedStage()
}

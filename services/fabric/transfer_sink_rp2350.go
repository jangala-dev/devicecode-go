//go:build tinygo && rp2350

package fabric

import (
	"errors"

	"devicecode-go/services/updater"
)

type streamedStageSink struct {
	accepted uint32
	closed   bool
}

func beginTransfer(meta transferMeta) (transferSink, error) {
	if err := updater.BeginStreamedStage(meta.Size); err != nil {
		return nil, err
	}
	return &streamedStageSink{}, nil
}

func (s *streamedStageSink) WriteChunk(off uint32, data []byte) error {
	if s.closed {
		return errors.New("sink_closed")
	}
	if s.accepted != off {
		return errors.New("unexpected_offset")
	}
	if err := updater.WriteStreamedStage(data); err != nil {
		return err
	}
	s.accepted += uint32(len(data))
	return nil
}

func (s *streamedStageSink) Commit() (transferInfo, error) {
	if s.closed {
		return transferInfo{}, errors.New("sink_closed")
	}
	written, err := updater.CommitStreamedStage()
	if err != nil {
		return transferInfo{}, err
	}
	s.closed = true
	return transferInfo{BytesWritten: written}, nil
}

func (s *streamedStageSink) Apply() error { return nil }

func (s *streamedStageSink) Abort(reason string) error {
	_ = reason
	updater.AbortStreamedStage()
	s.closed = true
	return nil
}

// Bytes returns nil because the TinyGo RP2350 default path streams directly
// into the inactive slot. fabric still calls updater/main staging; the updater
// consumes the pre-staged descriptor instead of an in-RAM artefact.
func (s *streamedStageSink) Bytes() []byte { return nil }

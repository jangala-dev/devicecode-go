//go:build !(tinygo && rp2350)

package fabric

import (
	"errors"

	"devicecode-go/services/updater"
)

// streamedStageSink is also the host/dev default. It does not retain the
// transfer as a whole-image []byte in Fabric. Host builds stream into the
// updater's host pre-stage implementation; RP2350 builds use the hardware
// implementation in transfer_sink_rp2350.go.
type streamedStageSink struct {
	xferID     string
	generation uint64
	accepted   uint32
	closed     bool
}

func beginTransfer(meta transferMeta) (transferSink, error) {
	generation, err := updater.BeginStreamedStage(meta.ID, meta.Size)
	if err != nil {
		return nil, err
	}
	return &streamedStageSink{xferID: meta.ID, generation: generation}, nil
}

func (s *streamedStageSink) WriteChunk(off uint32, data []byte) error {
	if s.closed {
		return errors.New("sink_closed")
	}
	if s.accepted != off {
		return errors.New("unexpected_offset")
	}
	if err := updater.WriteStreamedStage(s.xferID, s.generation, data); err != nil {
		return err
	}
	s.accepted += uint32(len(data))
	return nil
}

func (s *streamedStageSink) Commit() (transferInfo, error) {
	if s.closed {
		return transferInfo{}, errors.New("sink_closed")
	}
	written, err := updater.CommitStreamedStage(s.xferID, s.generation)
	if err != nil {
		return transferInfo{}, err
	}
	s.closed = true
	return transferInfo{BytesWritten: written, Generation: s.generation}, nil
}

func (s *streamedStageSink) Apply() error { return nil }

func (s *streamedStageSink) Abort(reason string) error {
	updater.AbortStreamedStage(s.xferID, s.generation, reason)
	s.closed = true
	return nil
}

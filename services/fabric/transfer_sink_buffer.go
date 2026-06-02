package fabric

import (
	"errors"

	"devicecode-go/services/updater"
)

// bufferSink is the default in-memory transferSink: it buffers the
// verified-by-wire (xxHash32) artefact in RAM and exposes the bytes via
// Bytes() so onTransferCommit can hand them to the updater/main staging
// RPC. The updater is responsible for signed-image verification and staging.
//
// Size cap is deliberately conservative: the smoke tests target small
// artefacts and large firmware images need a streaming-into-flash sink.
// Hitting the cap aborts the transfer cleanly via WriteChunk ->
// ErrArtefactTooLarge.
const maxArtefactBytes = 64 * 1024

var ErrArtefactTooLarge = errors.New("artefact_too_large")

type bufferSink struct {
	meta       transferMeta
	generation uint64
	buf        []byte
	closed     bool
	committed  bool
}

func newBufferSink(meta transferMeta) (*bufferSink, error) {
	generation, err := updater.BeginStreamedStage(meta.ID, meta.Size)
	if err != nil {
		return nil, err
	}
	return &bufferSink{
		meta:       meta,
		generation: generation,
		buf:        make([]byte, 0, sizeHint(meta.Size)),
	}, nil
}

func sizeHint(announced uint32) int {
	if announced == 0 || announced > maxArtefactBytes {
		return maxArtefactBytes
	}
	return int(announced)
}

func (s *bufferSink) WriteChunk(off uint32, data []byte) error {
	if s.closed {
		return errors.New("sink_closed")
	}
	if int(off) != len(s.buf) {
		return errors.New("unexpected_offset")
	}
	if len(s.buf)+len(data) > maxArtefactBytes {
		return ErrArtefactTooLarge
	}
	s.buf = append(s.buf, data...)
	return nil
}

func (s *bufferSink) Commit() (transferInfo, error) {
	if s.closed {
		return transferInfo{}, errors.New("sink_closed")
	}
	if s.generation != 0 {
		if err := updater.CommitBufferedStage(s.meta.ID, s.generation); err != nil {
			return transferInfo{}, err
		}
	}
	s.committed = true
	return transferInfo{BytesWritten: uint32(len(s.buf)), Generation: s.generation}, nil
}

// Apply is a no-op for the buffer sink — the staged-image apply
// (slot switch + reboot) belongs to the updater's commit RPC, not to
// fabric's transfer state machine.
func (s *bufferSink) Apply() error { return nil }

func (s *bufferSink) Abort(reason string) error {
	if s.generation != 0 {
		updater.AbortStreamedStage(s.meta.ID, s.generation, reason)
	}
	s.buf = nil
	s.closed = true
	return nil
}

func (s *bufferSink) Bytes() []byte {
	if !s.committed {
		return nil
	}
	return s.buf
}

package fabric

import "errors"

// bufferSink is the default transferSink for the fabric-update branch:
// it buffers the verified-by-wire (xxHash32) artefact in RAM and exposes
// the bytes via Bytes() so onTransferCommit can hand them to the
// updater/main staging RPC. The updater is responsible for signed-image
// verification and staging.
//
// Size cap is deliberately conservative: the smoke tests target small
// artefacts and large firmware images need a streaming-into-flash
// sink, which is fabric-security's job. Hitting the cap aborts the
// transfer cleanly via WriteChunk -> ErrArtefactTooLarge.
const maxArtefactBytes = 64 * 1024

var ErrArtefactTooLarge = errors.New("artefact_too_large")

type bufferSink struct {
	meta      transferMeta
	buf       []byte
	closed    bool
	committed bool
}

func newBufferSink(meta transferMeta) *bufferSink {
	return &bufferSink{
		meta: meta,
		buf:  make([]byte, 0, sizeHint(meta.Size)),
	}
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
	s.committed = true
	return transferInfo{BytesWritten: uint32(len(s.buf))}, nil
}

// Apply is a no-op for the buffer sink — the staged-image apply
// (slot switch + reboot) belongs to the updater's commit RPC, not to
// fabric's transfer state machine. fabric-security wires the real
// apply path through `cap/self/updater/main/rpc/commit-update`.
func (s *bufferSink) Apply() error { return nil }

func (s *bufferSink) Abort(reason string) error {
	_ = reason
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

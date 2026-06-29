//go:build !tinygo

package updater

import (
	"bytes"
	"io"
)

// memorySink buffers verified payload bytes in RAM. Used on host builds
// (tests, dev builds) where there's no flash slot to write into. Any
// build that needs to stage to actual flash uses the tinygo+rp2350
// sink in sink_tinygo.go.
type memorySink struct {
	buf    bytes.Buffer
	closed bool
}

func (m *memorySink) Write(p []byte) (int, error) {
	if m.closed {
		return 0, io.ErrClosedPipe
	}
	return m.buf.Write(p)
}

func (m *memorySink) Commit() error {
	m.closed = true
	return nil
}

func (m *memorySink) Abort() error {
	m.buf.Reset()
	m.closed = true
	return nil
}

// newSlotSink returns the host-default sink. totalSize is unused — the
// memory sink grows as bytes arrive. Staging passes it for parity
// with the tinygo factory which must hand the size to abupdate up
// front.
func newSlotSink(totalSize uint32) (SlotSink, error) {
	_ = totalSize
	return &memorySink{}, nil
}

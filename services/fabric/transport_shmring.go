package fabric

import (
	"context"
	"fmt"

	"devicecode-go/x/shmring"
)

// ShmringTransport implements Transport over two shmring rings (RX + TX).
// Used for UART0 in production (main.go).
type ShmringTransport struct {
	rx     *shmring.Ring
	tx     *shmring.Ring
	cancel context.CancelFunc
	ctx    context.Context
	n      int
	over   bool // draining an oversize line
}

func NewShmringTransport(rx, tx *shmring.Ring) *ShmringTransport {
	return NewShmringTransportWithBuffers(rx, tx, nil)
}

func NewShmringTransportWithBuffers(rx, tx *shmring.Ring, buffers *FabricBuffers) *ShmringTransport {
	ctx, cancel := context.WithCancel(context.Background())
	_ = buffers // retained for API compatibility with callers that own FabricBuffers.
	return &ShmringTransport{rx: rx, tx: tx, cancel: cancel, ctx: ctx}
}

func (t *ShmringTransport) ReadLine() ([]byte, error) {
	var tmp [maxLineLen]byte
	n, err := t.ReadLineInto(tmp[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, tmp[:n])
	return out, nil
}

func (t *ShmringTransport) ReadLineInto(dst []byte) (int, error) {
	if len(dst) < maxLineLen {
		return 0, fmt.Errorf("fabric read buffer too small: %d", len(dst))
	}
	t.n = 0
	t.over = false

	for {
		p1, p2 := t.rx.ReadAcquire()
		if len(p1)+len(p2) == 0 {
			select {
			case <-t.ctx.Done():
				return 0, fmt.Errorf("transport closed")
			case <-t.rx.Readable():
				continue
			}
		}

		if idx := findByte(p1, '\n'); idx >= 0 {
			if !t.over && !t.appendLineChunk(dst, p1[:idx]) {
				t.over = true
			}
			t.rx.ReadRelease(idx + 1)
			return t.finishLineInto(dst)
		}

		if !t.over && !t.appendLineChunk(dst, p1) {
			t.over = true
		}

		if idx := findByte(p2, '\n'); idx >= 0 {
			if !t.over && !t.appendLineChunk(dst, p2[:idx]) {
				t.over = true
			}
			t.rx.ReadRelease(len(p1) + idx + 1)
			return t.finishLineInto(dst)
		}

		if !t.over && !t.appendLineChunk(dst, p2) {
			t.over = true
		}
		t.rx.ReadRelease(len(p1) + len(p2))
	}
}

func (t *ShmringTransport) appendLineChunk(dst []byte, p []byte) bool {
	if len(p) == 0 {
		return true
	}
	if t.n+len(p) > maxLineLen {
		t.n = 0
		return false
	}
	copy(dst[t.n:], p)
	t.n += len(p)
	return true
}

func (t *ShmringTransport) finishLineInto(dst []byte) (int, error) {
	if t.over {
		t.n = 0
		t.over = false
		return 0, ErrLineTooLong
	}
	return t.n, nil
}

func (t *ShmringTransport) WriteLine(data []byte) error {
	if len(data) > maxLineLen {
		return ErrLineTooLong
	}
	if err := t.writeBytes(data); err != nil {
		return err
	}
	if err := t.writeBytes([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}

func (t *ShmringTransport) writeBytes(data []byte) error {
	written := 0
	for written < len(data) {
		p1, p2 := t.tx.WriteAcquire()
		if len(p1)+len(p2) == 0 {
			select {
			case <-t.ctx.Done():
				return fmt.Errorf("transport closed")
			case <-t.tx.Writable():
				continue
			}
		}
		remaining := data[written:]
		n := copy(p1, remaining)
		remaining = remaining[n:]
		if len(remaining) > 0 && len(p2) > 0 {
			n += copy(p2, remaining)
		}
		t.tx.WriteCommit(n)
		written += n
	}
	return nil
}

func (t *ShmringTransport) Close() error {
	t.cancel()
	return nil
}

func findByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

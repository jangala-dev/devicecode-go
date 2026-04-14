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
	buf    []byte
	over   bool // draining an oversize line
}

func NewShmringTransport(rx, tx *shmring.Ring) *ShmringTransport {
	ctx, cancel := context.WithCancel(context.Background())
	return &ShmringTransport{
		rx:     rx,
		tx:     tx,
		cancel: cancel,
		ctx:    ctx,
		buf:    make([]byte, 0, 256),
	}
}

func (t *ShmringTransport) ReadLine() ([]byte, error) {
	t.buf = t.buf[:0]
	t.over = false

	for {
		p1, p2 := t.rx.ReadAcquire()
		if len(p1)+len(p2) == 0 {
			select {
			case <-t.ctx.Done():
				return nil, fmt.Errorf("transport closed")
			case <-t.rx.Readable():
				continue
			}
		}

		// Scan p1 for newline.
		if idx := findByte(p1, '\n'); idx >= 0 {
			if !t.over {
				t.buf = append(t.buf, p1[:idx]...)
			}
			t.rx.ReadRelease(idx + 1)
			if t.over {
				t.buf = t.buf[:0]
				t.over = false
				return nil, ErrLineTooLong
			}
			if len(t.buf) > maxLineLen {
				return nil, ErrLineTooLong
			}
			out := make([]byte, len(t.buf))
			copy(out, t.buf)
			traceLine("rx", out)
			return out, nil
		}

		// No newline in p1 — consume it, check p2.
		if !t.over {
			t.buf = append(t.buf, p1...)
		}

		if idx := findByte(p2, '\n'); idx >= 0 {
			if !t.over {
				t.buf = append(t.buf, p2[:idx]...)
			}
			t.rx.ReadRelease(len(p1) + idx + 1)
			if t.over {
				t.buf = t.buf[:0]
				t.over = false
				return nil, ErrLineTooLong
			}
			if len(t.buf) > maxLineLen {
				return nil, ErrLineTooLong
			}
			out := make([]byte, len(t.buf))
			copy(out, t.buf)
			traceLine("rx", out)
			return out, nil
		}

		// No newline — consume everything, wait for more.
		if !t.over {
			t.buf = append(t.buf, p2...)
		}
		t.rx.ReadRelease(len(p1) + len(p2))

		// Check for oversize.
		if len(t.buf) > maxLineLen {
			t.buf = t.buf[:0]
			t.over = true
		}
	}
}

func (t *ShmringTransport) WriteLine(data []byte) error {
	if len(data) > maxLineLen {
		return ErrLineTooLong
	}
	line := append(data, '\n')
	written := 0

	for written < len(line) {
		p1, p2 := t.tx.WriteAcquire()
		if len(p1)+len(p2) == 0 {
			select {
			case <-t.ctx.Done():
				return fmt.Errorf("transport closed")
			case <-t.tx.Writable():
				continue
			}
		}

		remaining := line[written:]
		n := copy(p1, remaining)
		remaining = remaining[n:]
		if len(remaining) > 0 && len(p2) > 0 {
			n += copy(p2, remaining)
		}
		t.tx.WriteCommit(n)
		written += n
	}
	traceLine("tx", data)
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

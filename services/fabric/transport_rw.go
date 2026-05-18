package fabric

import (
	"bufio"
	"fmt"
	"io"
	"sync"
)

// Used by host-side unit tests and any stream-backed Fabric transport.

// maxLineLen caps a single fabric frame (line-delimited JSON) end-to-end. It
// must clear the worst-case encoded transfer chunk: release chunk_size = 2048
// raw → ~2731 chars base64url-encoded + ~150-byte JSON envelope + newline
// ≈ 2900 bytes. 4096 is the tightest round power-of-2 above that with ~1.1 KB
// headroom. See devicecode-lua/src/services/fabric/protocol.lua at
// update-migration tip for the canonical encoding.
const maxLineLen = 4096

var ErrLineTooLong = fmt.Errorf("line exceeds %d bytes", maxLineLen)

// RWTransport implements Transport over an io.Reader + io.Writer.
type RWTransport struct {
	r       *bufio.Reader
	mu      sync.Mutex
	w       *bufio.Writer
	closers []io.Closer
}

func NewRWTransport(r io.Reader, w io.Writer) *RWTransport {
	t := &RWTransport{
		r: bufio.NewReaderSize(r, maxLineLen),
		w: bufio.NewWriter(w),
	}
	var rc io.Closer
	if c, ok := r.(io.Closer); ok {
		rc = c
		t.closers = append(t.closers, c)
	}
	if c, ok := w.(io.Closer); ok {
		if c != rc {
			t.closers = append(t.closers, c)
		}
	}
	return t
}

func (t *RWTransport) ReadLine() ([]byte, error) {
	var buf []byte
	for {
		seg, more, err := t.r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, seg...)
		if !more {
			break
		}
		if len(buf) > maxLineLen {
			for more {
				_, more, err = t.r.ReadLine()
				if err != nil {
					return nil, err
				}
			}
			return nil, ErrLineTooLong
		}
	}
	if len(buf) > maxLineLen {
		return nil, ErrLineTooLong
	}
	traceLine("rx", buf)
	return buf, nil
}

func (t *RWTransport) WriteLine(data []byte) error {
	if len(data) > maxLineLen {
		return ErrLineTooLong
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.w.Write(data); err != nil {
		return err
	}
	if err := t.w.WriteByte('\n'); err != nil {
		return err
	}
	if err := t.w.Flush(); err != nil {
		return err
	}
	traceLine("tx", data)
	return nil
}

func (t *RWTransport) Close() error {
	var first error
	for _, c := range t.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

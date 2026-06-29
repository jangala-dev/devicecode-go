package fabric

import (
	"bufio"
	"io"
	"sync"
)

type rwTransport struct {
	r       *bufio.Reader
	mu      sync.Mutex
	w       *bufio.Writer
	closers []io.Closer
}

func newRWTransport(r io.Reader, w io.Writer) *rwTransport {
	t := &rwTransport{
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

func (t *rwTransport) ReadLine() ([]byte, error) {
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
	return buf, nil
}

func (t *rwTransport) WriteLine(data []byte) error {
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
	return t.w.Flush()
}

func (t *rwTransport) Close() error {
	var first error
	for _, c := range t.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

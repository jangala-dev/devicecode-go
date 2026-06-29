//go:build !tinygo

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const devhostMaxLineLen = 64 * 1024

// lineTransport adapts a host POSIX tty, pipe, or socket to Fabric's JSONL
// transport interface. It deliberately does not inject noise; the Lua test rig
// owns stream pressure and fragmentation so there is one place to reason about
// line conditions.
type lineTransport struct {
	rwc io.ReadWriteCloser
	r   *bufio.Reader
	mu  sync.Mutex
}

func openLineTransport(path string) (*lineTransport, error) {
	if path == "" {
		return nil, errors.New("uart path required")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return newLineTransport(f), nil
}

func newLineTransport(rwc io.ReadWriteCloser) *lineTransport {
	return &lineTransport{rwc: rwc, r: bufio.NewReaderSize(rwc, devhostMaxLineLen)}
}

func (t *lineTransport) ReadLine() ([]byte, error) {
	if t == nil || t.r == nil {
		return nil, errors.New("transport closed")
	}
	line, err := t.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if len(line) > devhostMaxLineLen {
		return nil, fmt.Errorf("line too long: %d", len(line))
	}
	out := make([]byte, len(line))
	copy(out, line)
	return out, nil
}

func (t *lineTransport) WriteLine(data []byte) error {
	if len(data) > devhostMaxLineLen {
		return fmt.Errorf("line too long: %d", len(data))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	line := make([]byte, 0, len(data)+1)
	line = append(line, data...)
	line = append(line, byte(10))
	_, err := t.rwc.Write(line)
	return err
}
func (t *lineTransport) Close() error {
	if t == nil || t.rwc == nil {
		return nil
	}
	return t.rwc.Close()
}

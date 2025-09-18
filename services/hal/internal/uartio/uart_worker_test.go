package uartio

import (
	"context"
	"sync"
	"testing"
	"time"
)

// --- minimal fake UART implementing halcore.UARTPort ---

type fakeUART struct {
	mu sync.Mutex
	rx []byte
	rd chan struct{}
}

func newFakeUART() *fakeUART { return &fakeUART{rd: make(chan struct{}, 1)} }

func (f *fakeUART) inject(b []byte) {
	f.mu.Lock()
	f.rx = append(f.rx, b...)
	if len(f.rd) == 0 {
		f.rd <- struct{}{}
	}
	f.mu.Unlock()
}

// halcore.UARTPort
func (f *fakeUART) WriteByte(byte) error        { return nil }
func (f *fakeUART) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeUART) Buffered() int               { f.mu.Lock(); n := len(f.rx); f.mu.Unlock(); return n }
func (f *fakeUART) Read(p []byte) (int, error) {
	f.mu.Lock()
	n := copy(p, f.rx)
	f.rx = f.rx[n:]
	f.mu.Unlock()
	return n, nil
}
func (f *fakeUART) Readable() <-chan struct{} { return f.rd }
func (f *fakeUART) RecvSomeContext(ctx context.Context, p []byte) (int, error) {
	if n := f.Buffered(); n > 0 {
		return f.Read(p)
	}
	select {
	case <-f.rd:
		return f.Read(p)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// --- helpers ---

func recvEvent(ch <-chan *Event, d time.Duration) (*Event, bool) {
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(d):
		return nil, false
	}
}

// --- tests ---

func TestUARTWorker_BytesMode_EmitAndRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := newFakeUART()
	w := New(8)
	stop, err := w.Register(ctx, ReaderCfg{
		DevID:    "u1",
		Port:     u,
		Mode:     "bytes",
		MaxFrame: 16,
	})
	if err != nil {
		t.Errorf("Register: %v", err)
		return
	}
	defer stop()

	u.inject([]byte("abc"))
	ev, ok := recvEvent(w.Events(), time.Second)
	if !ok {
		t.Errorf("timeout waiting for rx")
		return
	}
	if ev.DevID != "u1" || ev.Dir != "rx" {
		t.Errorf("unexpected meta: %+v", *ev)
	}
	if string(ev.Data) != "abc" {
		t.Errorf("unexpected data: %q", string(ev.Data))
	}
	if ev.TS.IsZero() {
		t.Errorf("timestamp not set")
	}
	if cap(ev.Data) != 16 {
		t.Errorf("expected cap=16, got %d", cap(ev.Data))
	}
	ev.Release()

	u.inject([]byte("xyz123"))
	ev2, ok := recvEvent(w.Events(), time.Second)
	if !ok {
		t.Errorf("timeout waiting for rx 2")
		return
	}
	if string(ev2.Data) != "xyz123" {
		t.Errorf("unexpected data 2: %q", string(ev2.Data))
	}
	ev2.Release()
}

func TestUARTWorker_LinesMode_NewlineAndIdleFlush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := newFakeUART()
	w := New(8)
	stop, err := w.Register(ctx, ReaderCfg{
		DevID:     "u2",
		Port:      u,
		Mode:      "lines",
		MaxFrame:  32,
		IdleFlush: 30 * time.Millisecond,
	})
	if err != nil {
		t.Errorf("Register: %v", err)
		return
	}
	defer stop()

	u.inject([]byte("a"))
	ev, ok := recvEvent(w.Events(), 300*time.Millisecond)
	if !ok {
		t.Errorf("idle flush timeout")
		return
	}
	if got := string(ev.Data); got != "a" {
		t.Errorf("idle flush got %q want %q", got, "a")
	}
	ev.Release()

	u.inject([]byte("hi\r\nthere\n"))
	ev, ok = recvEvent(w.Events(), time.Second)
	if !ok {
		t.Errorf("line 1 timeout")
		return
	}
	if got := string(ev.Data); got != "hi" {
		t.Errorf("line 1 got %q want %q", got, "hi")
	}
	ev.Release()

	ev, ok = recvEvent(w.Events(), time.Second)
	if !ok {
		t.Errorf("line 2 timeout")
		return
	}
	if got := string(ev.Data); got != "there" {
		t.Errorf("line 2 got %q want %q", got, "there")
	}
	ev.Release()

	u.inject([]byte("z"))
	ev, ok = recvEvent(w.Events(), 300*time.Millisecond)
	if !ok {
		t.Errorf("idle flush 2 timeout")
		return
	}
	if got := string(ev.Data); got != "z" {
		t.Errorf("idle flush 2 got %q want %q", got, "z")
	}
	ev.Release()
}

func TestUARTWorker_TxEchoChunking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := newFakeUART()
	w := New(4)
	stop, err := w.Register(ctx, ReaderCfg{
		DevID:    "u3",
		Port:     u,
		Mode:     "bytes",
		MaxFrame: 8, // relies on worker allowing 8-byte frames
	})
	if err != nil {
		t.Errorf("Register: %v", err)
		return
	}
	defer stop()

	src := []byte("ABCDEFGHIJKLMNOPQRST") // 20 bytes
	w.EmitTX("u3", src)

	ev1, ok := recvEvent(w.Events(), time.Second)
	if !ok {
		t.Errorf("chunk1 timeout")
		return
	}
	if ev1.Dir != "tx" {
		t.Errorf("dir1 = %s", ev1.Dir)
	}
	if string(ev1.Data) != "ABCDEFGH" {
		t.Errorf("chunk1 got %q", string(ev1.Data))
	}
	ev1.Release()

	ev2, ok := recvEvent(w.Events(), time.Second)
	if !ok {
		t.Errorf("chunk2 timeout")
		return
	}
	if string(ev2.Data) != "IJKLMNOP" {
		t.Errorf("chunk2 got %q", string(ev2.Data))
	}
	ev2.Release()

	ev3, ok := recvEvent(w.Events(), time.Second)
	if !ok {
		t.Errorf("chunk3 timeout")
		return
	}
	if string(ev3.Data) != "QRST" {
		t.Errorf("chunk3 got %q", string(ev3.Data))
	}
	ev3.Release()
}

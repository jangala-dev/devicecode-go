package serial_raw

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"devicecode-go/services/hal/internal/core"
	"devicecode-go/types"
)

type fakeSerialPort struct {
	readable chan struct{}
	writable chan struct{}

	continuousRX atomic.Bool
	writeCalls   atomic.Int32
	readCalls    atomic.Int32
	maxReadLen   atomic.Int32
	maxWriteLen  atomic.Int32

	mu      sync.Mutex
	written []byte
}

func newFakeSerialPort() *fakeSerialPort {
	p := &fakeSerialPort{
		readable: make(chan struct{}, 1),
		writable: make(chan struct{}, 1),
	}
	p.signalReadable()
	p.signalWritable()
	return p
}

func (p *fakeSerialPort) TryRead(dst []byte) int {
	p.readCalls.Add(1)
	recordMax(&p.maxReadLen, len(dst))
	if !p.continuousRX.Load() || len(dst) == 0 {
		return 0
	}
	for i := range dst {
		dst[i] = 'r'
	}
	p.signalReadable()
	return len(dst)
}

func (p *fakeSerialPort) TryWrite(src []byte) int {
	p.writeCalls.Add(1)
	recordMax(&p.maxWriteLen, len(src))
	if len(src) == 0 {
		return 0
	}
	p.mu.Lock()
	p.written = append(p.written, src...)
	p.mu.Unlock()
	p.signalWritable()
	return len(src)
}

func (p *fakeSerialPort) Readable() <-chan struct{} { return p.readable }
func (p *fakeSerialPort) Writable() <-chan struct{} { return p.writable }
func (p *fakeSerialPort) Flush() error              { return nil }

func (p *fakeSerialPort) signalReadable() {
	select {
	case p.readable <- struct{}{}:
	default:
	}
}

func (p *fakeSerialPort) signalWritable() {
	select {
	case p.writable <- struct{}{}:
	default:
	}
}

func (p *fakeSerialPort) writtenBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]byte, len(p.written))
	copy(out, p.written)
	return out
}

func recordMax(max *atomic.Int32, n int) {
	for {
		cur := max.Load()
		if int32(n) <= cur {
			return
		}
		if max.CompareAndSwap(cur, int32(n)) {
			return
		}
	}
}

func newTestDevice(port *fakeSerialPort) *Device {
	return &Device{
		id:   "uart1_raw",
		a:    core.CapAddr{Domain: "io", Kind: types.KindSerial, Name: "uart1"},
		port: port,
	}
}

func drainRXUntil(ctx context.Context, s *session) {
	var buf [128]byte
	for {
		if s.rxRing.TryReadInto(buf[:]) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.rxRing.Readable():
		}
	}
}

func waitUntil(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestReactorServicesTXWhileRXIsContinuous(t *testing.T) {
	port := newFakeSerialPort()
	port.continuousRX.Store(true)
	dev := newTestDevice(port)
	dev.startSession(512, 512)

	drainCtx, stopDrain := context.WithCancel(context.Background())
	defer stopDrain()
	go drainRXUntil(drainCtx, dev.sess)

	payload := []byte("tx while rx is busy")
	if n := dev.sess.txRing.TryWriteFrom(payload); n != len(payload) {
		t.Fatalf("failed to seed tx ring: wrote %d/%d", n, len(payload))
	}

	waitUntil(t, 100*time.Millisecond, func() bool {
		return port.writeCalls.Load() > 0
	})

	if got := string(port.writtenBytes()); got != string(payload) {
		t.Fatalf("written payload mismatch: got %q want %q", got, payload)
	}
	if max := port.maxReadLen.Load(); max > serialRawPumpRXBudget {
		t.Fatalf("TryRead span exceeded budget: got %d want <= %d", max, serialRawPumpRXBudget)
	}
	if max := port.maxWriteLen.Load(); max > serialRawPumpTXBudget {
		t.Fatalf("TryWrite span exceeded budget: got %d want <= %d", max, serialRawPumpTXBudget)
	}

	dev.stopSession()
}

func TestStopSessionReturnsUnderContinuousRX(t *testing.T) {
	port := newFakeSerialPort()
	port.continuousRX.Store(true)
	dev := newTestDevice(port)
	dev.startSession(512, 512)

	drainCtx, stopDrain := context.WithCancel(context.Background())
	defer stopDrain()
	go drainRXUntil(drainCtx, dev.sess)

	done := make(chan struct{})
	go func() {
		dev.stopSession()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("stopSession did not return under continuous RX")
	}
}

func TestStopSessionReturnsWhenIdle(t *testing.T) {
	dev := newTestDevice(newFakeSerialPort())
	dev.startSession(512, 512)

	done := make(chan struct{})
	go func() {
		dev.stopSession()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("stopSession did not return while idle")
	}
}

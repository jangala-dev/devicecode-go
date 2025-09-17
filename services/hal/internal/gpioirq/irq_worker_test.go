// services/hal/internal/gpioirq/irq_worker_test.go

package gpioirq

import (
	"context"
	"sync"
	"testing"
	"time"

	"devicecode-go/services/hal/internal/halcore"
)

// fakeIRQPin implements halcore.IRQPin with minimal behaviour for tests.
type fakeIRQPin struct {
	mu      sync.Mutex
	level   bool
	handler func()
	number  int
}

func (p *fakeIRQPin) ConfigureInput(_ halcore.Pull) error   { return nil }
func (p *fakeIRQPin) ConfigureOutput(initial bool) error    { p.level = initial; return nil }
func (p *fakeIRQPin) Set(b bool)                            { p.mu.Lock(); p.level = b; p.mu.Unlock() }
func (p *fakeIRQPin) Get() bool                             { p.mu.Lock(); defer p.mu.Unlock(); return p.level }
func (p *fakeIRQPin) Toggle()                               { p.mu.Lock(); p.level = !p.level; p.mu.Unlock() }
func (p *fakeIRQPin) Number() int                           { return p.number }
func (p *fakeIRQPin) SetIRQ(_ halcore.Edge, h func()) error { p.handler = h; return nil }
func (p *fakeIRQPin) ClearIRQ() error                       { p.handler = nil; return nil }
func (p *fakeIRQPin) fire(level bool) {
	p.Set(level)
	if p.handler != nil {
		p.handler()
	}
}

func TestIRQWorkerDebounceAndEdges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := New(8, 8)
	w.Start(ctx)

	pin := &fakeIRQPin{number: 5}
	cancelReg, err := w.RegisterInput("dev1", pin, halcore.EdgeBoth, 10 /*ms*/, false /*invert*/)
	if err != nil {
		t.Fatalf("RegisterInput: %v", err)
	}
	defer cancelReg()

	// Initial level is false due to zero value.

	pin.fire(true) // rising
	select {
	case ev := <-w.Events():
		if ev.DevID != "dev1" || ev.Level != 1 || ev.Edge != halcore.EdgeRising {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for rising event")
	}

	// Within debounce window – should be suppressed.
	pin.fire(false)
	select {
	case <-w.Events():
		t.Fatal("unexpected event during debounce")
	case <-time.After(5 * time.Millisecond):
	}

	time.Sleep(12 * time.Millisecond) // exceed debounce

	pin.fire(false) // falling after debounce
	select {
	case ev := <-w.Events():
		if ev.Level != 0 || ev.Edge != halcore.EdgeFalling {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for falling event")
	}
}

func TestIRQWorkerInvert(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := New(8, 8)
	w.Start(ctx)

	pin := &fakeIRQPin{number: 7}
	cancelReg, err := w.RegisterInput("devX", pin, halcore.EdgeBoth, 0, true /*invert*/)
	if err != nil {
		t.Fatalf("RegisterInput: %v", err)
	}
	defer cancelReg()

	pin.fire(true) // physical high -> logical low due to invert
	select {
	case ev := <-w.Events():
		if ev.Level != 0 {
			t.Fatalf("expected inverted level 0, got %d", ev.Level)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for inverted event")
	}
}

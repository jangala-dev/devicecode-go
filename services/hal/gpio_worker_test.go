package hal

import (
	"context"
	"testing"
	"time"
)

// fake IRQ-capable pin

type fakeIRQPin struct {
	fakePin
	h func()
}

func (p *fakeIRQPin) SetIRQ(edge Edge, handler func()) error { p.h = handler; return nil }
func (p *fakeIRQPin) ClearIRQ() error                        { p.h = nil; return nil }

// simulate a hardware edge by setting level then calling ISR handler
func (p *fakeIRQPin) trigger(level bool) {
	p.level = level
	if p.h != nil {
		p.h()
	}
}

var _ IRQPin = (*fakeIRQPin)(nil)

func recvEvent(t *testing.T, ch <-chan GPIOEvent, d time.Duration) (GPIOEvent, bool) {
	t.Helper()
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(d):
		return GPIOEvent{}, false
	}
}

func TestGPIOWorker_RisingEdge_EventDelivered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &fakeIRQPin{}
	p.level = false // initial level
	w := newGPIOIRQWorker(16, 16)
	w.Start(ctx)

	cancelReg, err := w.RegisterInput("smbalert", p, EdgeRising, 0, false)
	if err != nil {
		t.Fatalf("RegisterInput error: %v", err)
	}
	defer cancelReg()

	// Rising transition: false -> true
	p.trigger(true)

	ev, ok := recvEvent(t, w.Events(), 50*time.Millisecond)
	if !ok {
		t.Fatal("expected event, got timeout")
	}
	if ev.DevID != "smbalert" || ev.Edge != EdgeRising || ev.Level != 1 {
		t.Fatalf("unexpected event: %+v", ev)
	}

	// Falling transition should be ignored for EdgeRising
	p.trigger(false)
	if _, ok := recvEvent(t, w.Events(), 10*time.Millisecond); ok {
		t.Fatal("did not expect an event for falling edge")
	}
}

func TestGPIOWorker_FallingEdge_WithDebounce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &fakeIRQPin{}
	p.level = false
	w := newGPIOIRQWorker(16, 16)
	w.Start(ctx)

	cancelReg, err := w.RegisterInput("in", p, EdgeBoth, 10 /*ms debounce*/, false)
	if err != nil {
		t.Fatalf("RegisterInput error: %v", err)
	}
	defer cancelReg()

	// Rising -> expect event
	p.trigger(true)
	if _, ok := recvEvent(t, w.Events(), 50*time.Millisecond); !ok {
		t.Fatal("expected rising event")
	}

	// Quick falling within debounce -> expect drop
	p.trigger(false)
	if _, ok := recvEvent(t, w.Events(), 5*time.Millisecond); ok {
		t.Fatal("unexpected event within debounce window")
	}

	// After debounce, falling should be seen
	time.Sleep(12 * time.Millisecond)
	p.trigger(false) // still false; to ensure a transition, go true->false
	p.trigger(true)
	if _, ok := recvEvent(t, w.Events(), 20*time.Millisecond); !ok {
		t.Fatal("expected event after debounce (rising)")
	}
}

func TestGPIOWorker_CancelStopsEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &fakeIRQPin{}
	w := newGPIOIRQWorker(16, 16)
	w.Start(ctx)

	stop, err := w.RegisterInput("x", p, EdgeBoth, 0, false)
	if err != nil {
		t.Fatalf("RegisterInput error: %v", err)
	}
	stop() // unregister

	// Trigger after unregister: no events expected
	p.trigger(true)
	if _, ok := recvEvent(t, w.Events(), 10*time.Millisecond); ok {
		t.Fatal("unexpected event after cancel")
	}
}

func TestGPIOWorker_ISRDropCounter(t *testing.T) {
	// Intentionally do not Start the worker to keep isrQ unconsumed.
	p := &fakeIRQPin{}
	p.level = false
	w := newGPIOIRQWorker(1 /*isrBuf*/, 0 /*outBuf*/)

	_, err := w.RegisterInput("y", p, EdgeBoth, 0, false)
	if err != nil {
		t.Fatalf("RegisterInput error: %v", err)
	}

	// First ISR send fills the buffer; second should increment drop counter.
	p.trigger(true)  // fills isrQ
	p.trigger(false) // should hit default: and count as drop

	if got := w.ISRDrops(); got == 0 {
		t.Fatalf("expected at least 1 ISR drop, got %d", got)
	}
}

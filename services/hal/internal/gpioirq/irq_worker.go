// services/hal/internal/gpioirq/irq_worker.go
package gpioirq

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/util"
)

// GPIOEvent is delivered from the worker to the HAL service.
type GPIOEvent struct {
	DevID string
	Level int // 0/1 after inversion applied
	Edge  halcore.Edge
	TS    time.Time
}

type Worker struct {
	// Written by ISR; MUST NOT block the ISR:
	isrQ chan isrEvent
	// Consumed by the HAL service:
	outQ    chan GPIOEvent
	stopped chan struct{}

	mu     sync.RWMutex
	inputs map[string]*watch // devID -> watch

	drops uint32 // ISR drop counter
}

type isrEvent struct {
	devID string
	level bool // captured in ISR
}

type watch struct {
	devID     string
	pin       halcore.IRQPin
	edge      halcore.Edge
	debounce  time.Duration
	invert    bool
	lastLevel bool
	lastEvent time.Time
	cancelIRQ func()
}

func New(isrBuf, outBuf int) *Worker {
	if isrBuf <= 0 {
		isrBuf = 64
	}
	if outBuf <= 0 {
		outBuf = 64
	}
	return &Worker{
		isrQ:    make(chan isrEvent, isrBuf),
		outQ:    make(chan GPIOEvent, outBuf),
		stopped: make(chan struct{}),
		inputs:  map[string]*watch{},
	}
}

func (w *Worker) Start(ctx context.Context) {
	go func() {
		defer close(w.stopped)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-w.isrQ:
				w.handleISR(ev)
			}
		}
	}()
}

func (w *Worker) Events() <-chan GPIOEvent { return w.outQ }

func (w *Worker) RegisterInput(devID string, pin halcore.IRQPin, edge halcore.Edge, debounceMS int, invert bool) (func(), error) {
	if edge == halcore.EdgeNone {
		return func() {}, nil
	}
	deb := time.Duration(debounceMS) * time.Millisecond

	// Take the initial *logical* level snapshot (after inversion),
	// so that subsequent edge detection compares like-for-like.
	init := pin.Get()
	if invert {
		init = !init
	}
	wh := &watch{
		devID:     devID,
		pin:       pin,
		edge:      edge,
		debounce:  deb,
		invert:    invert,
		lastLevel: init, // initial logical snapshot
	}

	// ISR handler: fast register read + non-blocking channel send.
	handler := func() {
		l := pin.Get()
		select {
		case w.isrQ <- isrEvent{devID: devID, level: l}:
		default:
			atomic.AddUint32(&w.drops, 1) // protect ISR path
		}
	}
	if err := pin.SetIRQ(edge, handler); err != nil {
		return nil, err
	}
	wh.cancelIRQ = func() { _ = pin.ClearIRQ() }

	w.mu.Lock()
	w.inputs[devID] = wh
	w.mu.Unlock()

	return func() {
		w.mu.Lock()
		if cur, ok := w.inputs[devID]; ok {
			if cur.cancelIRQ != nil {
				cur.cancelIRQ()
			}
			delete(w.inputs, devID)
		}
		w.mu.Unlock()
	}, nil
}

func (w *Worker) handleISR(ev isrEvent) {
	w.mu.RLock()
	wh := w.inputs[ev.devID]
	w.mu.RUnlock()
	if wh == nil {
		return
	}
	raw := ev.level
	if wh.invert {
		raw = !raw
	}
	now := time.Now()

	// Debounce
	if !wh.lastEvent.IsZero() && now.Sub(wh.lastEvent) < wh.debounce {
		return
	}

	// Edge detection
	var e halcore.Edge
	if wh.edge == halcore.EdgeBoth {
		switch {
		case !wh.lastLevel && raw:
			e = halcore.EdgeRising
		case wh.lastLevel && !raw:
			e = halcore.EdgeFalling
		}
	} else {
		// We only get called when the configured edge fired;
		// trust the configuration for direction on first observation.
		switch wh.edge {
		case halcore.EdgeRising:
			e = halcore.EdgeRising
		case halcore.EdgeFalling:
			e = halcore.EdgeFalling
		}
	}

	// Emit if requested edge; drop if none (e == EdgeNone)
	if e != halcore.EdgeNone {
		select {
		case w.outQ <- GPIOEvent{DevID: ev.devID, Level: util.BoolToInt(raw), Edge: e, TS: now}:
		default:
			// drop to protect system if consumer is slow
		}
	}

	// Always update snapshots
	wh.lastLevel = raw
	wh.lastEvent = now
}

func (w *Worker) ISRDrops() uint32 { return atomic.LoadUint32(&w.drops) }

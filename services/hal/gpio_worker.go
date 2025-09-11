// services/hal/gpio_worker.go
package hal

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// GPIOEvent is delivered from the worker to the HAL service.
type GPIOEvent struct {
	DevID string
	Level int // 0/1 after inversion applied
	Edge  Edge
	TS    time.Time
}

type gpioIRQWorker struct {
	// Written by ISR; MUST NOT block the ISR:
	isrQ chan isrEvent

	// Consumed by the HAL service:
	outQ chan GPIOEvent

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
	pin       IRQPin
	edge      Edge
	debounce  time.Duration
	invert    bool
	lastLevel bool
	lastEvent time.Time
	cancelIRQ func()
}

func newGPIOIRQWorker(isrBuf, outBuf int) *gpioIRQWorker {
	if isrBuf <= 0 {
		isrBuf = 64
	}
	if outBuf <= 0 {
		outBuf = 64
	}
	return &gpioIRQWorker{
		isrQ:    make(chan isrEvent, isrBuf),
		outQ:    make(chan GPIOEvent, outBuf),
		stopped: make(chan struct{}),
		inputs:  map[string]*watch{},
	}
}

func (w *gpioIRQWorker) Start(ctx context.Context) {
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

func (w *gpioIRQWorker) Events() <-chan GPIOEvent { return w.outQ }

func (w *gpioIRQWorker) RegisterInput(devID string, pin IRQPin, edge Edge, debounceMS int, invert bool) (func(), error) {
	if edge == EdgeNone {
		return func() {}, nil
	}
	deb := time.Duration(debounceMS) * time.Millisecond

	wh := &watch{
		devID:     devID,
		pin:       pin,
		edge:      edge,
		debounce:  deb,
		invert:    invert,
		lastLevel: pin.Get(), // initial snapshot
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

func (w *gpioIRQWorker) handleISR(ev isrEvent) {
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
	var e Edge
	switch {
	case !wh.lastLevel && raw:
		e = EdgeRising
	case wh.lastLevel && !raw:
		e = EdgeFalling
	default:
		return
	}

	if wh.edge == EdgeBoth || wh.edge == e {
		select {
		case w.outQ <- GPIOEvent{DevID: ev.devID, Level: boolToInt(raw), Edge: e, TS: now}:
		default:
			// drop to protect system if consumer is slow
		}
	}

	wh.lastLevel = raw
	wh.lastEvent = now
}

func (w *gpioIRQWorker) ISRDrops() uint32 { return atomic.LoadUint32(&w.drops) }

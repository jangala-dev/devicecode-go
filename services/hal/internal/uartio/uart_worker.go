package uartio

import (
	"context"
	"sync"
	"time"

	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/util"
)

// Event is delivered to the HAL service.
// Data points to a pooled buffer in "bytes" mode and on TX echoes.
// Call Release() after handling to return the buffer to the pool.
type Event struct {
	DevID string
	Dir   string // "rx" | "tx"
	Data  []byte
	TS    time.Time

	// pool is nil when the buffer was not pooled (e.g. future variants).
	pool *bufPool
}

// Release returns the buffer to the pool (idempotent).
func (e *Event) Release() {
	if e.pool != nil && e.Data != nil {
		e.pool.put(e.Data)
		e.Data = nil
		e.pool = nil
	}
}

type ReaderCfg struct {
	DevID         string
	Port          halcore.UARTPort
	Mode          string        // "bytes" | "lines"
	MaxFrame      int           // clamp 16..256
	IdleFlush     time.Duration // clamp 0..2s (lines mode)
	PublishTXEcho bool          // noted by service; worker does not gate on this
}

type Worker struct {
	outQ chan *Event

	mu      sync.RWMutex
	readers map[string]*readerState // devID -> reader state (pool, cfg, cancel)
}

type readerState struct {
	cfg    ReaderCfg
	pool   *bufPool
	cancel context.CancelFunc
}

func New(outBuf int) *Worker {
	if outBuf <= 0 {
		outBuf = 64
	}
	return &Worker{
		outQ:    make(chan *Event, outBuf),
		readers: make(map[string]*readerState),
	}
}

func (w *Worker) Events() <-chan *Event { return w.outQ }

// Register starts a bounded reader goroutine for a UART port. Returns cancel.
func (w *Worker) Register(ctx context.Context, cfg ReaderCfg) (func(), error) {
	max := cfg.MaxFrame
	if max < 8 {
		max = 8
	}
	if max > 256 {
		max = 256
	}
	idle := cfg.IdleFlush
	if idle < 0 {
		idle = 0
	}
	if idle > 2*time.Second {
		idle = 2 * time.Second
	}

	cctx, cancel := context.WithCancel(ctx)

	// Pool depth equals outQ cap to bound RAM to cap(outQ)*MaxFrame per reader.
	pool := newBufPool(cap(w.outQ), max)

	rs := &readerState{cfg: cfg, pool: pool, cancel: cancel}
	w.mu.Lock()
	w.readers[cfg.DevID] = rs
	w.mu.Unlock()

	go func() {
		defer func() {
			// cleanup reader state
			w.mu.Lock()
			if cur, ok := w.readers[cfg.DevID]; ok && cur == rs {
				delete(w.readers, cfg.DevID)
			}
			w.mu.Unlock()
		}()

		// Scratch buffer for lines mode; accumulator lives on the goroutine stack.
		var line []byte

		timer := time.NewTimer(time.Hour)
		if !timer.Stop() {
			util.DrainTimer(timer)
		}

		flush := func(now time.Time) {
			if len(line) == 0 {
				return
			}
			b := pool.get()    // blocks if all buffers are in flight
			n := copy(b, line) // copy to pooled slab
			line = line[:0]    // keep accumulator
			ev := &Event{DevID: cfg.DevID, Dir: "rx", Data: b[:n], TS: now, pool: pool}
			select {
			case w.outQ <- ev:
			default:
				// drop under back-pressure; return buffer
				ev.Release()
			}
		}

		for {
			// Arm idle flush only when needed.
			if cfg.Mode == "lines" && len(line) > 0 && idle > 0 {
				util.ResetTimer(timer, idle)
			} else {
				util.ResetTimer(timer, time.Hour)
			}

			select {
			case <-cctx.Done():
				flush(time.Now()) // best-effort submit
				return

			case <-cfg.Port.Readable():
				// Bound the blocking wait to assist shutdown.
				rctx, rcancel := context.WithTimeout(cctx, 250*time.Millisecond)
				if cfg.Mode == "lines" {
					// Use a pooled slab as scratch; copy interesting bytes into 'line'.
					b := pool.get()
					n, _ := cfg.Port.RecvSomeContext(rctx, b)
					rcancel()
					if n <= 0 {
						pool.put(b)
						continue
					}
					now := time.Now()
					for i := 0; i < n; i++ {
						switch b[i] {
						case '\n':
							flush(now)
						case '\r':
							// ignore
						default:
							if len(line) < max {
								line = append(line, b[i])
							}
						}
					}
					pool.put(b) // return scratch slab
				} else {
					// "bytes" mode: zero-copy from driver into pooled slab, publish directly.
					b := pool.get()
					n, _ := cfg.Port.RecvSomeContext(rctx, b)
					rcancel()
					if n <= 0 {
						pool.put(b)
						continue
					}
					now := time.Now()
					ev := &Event{DevID: cfg.DevID, Dir: "rx", Data: b[:n], TS: now, pool: pool}
					select {
					case w.outQ <- ev:
					default:
						ev.Release()
					}
				}

			case <-timer.C:
				flush(time.Now())
			}
		}
	}()

	return func() {
		cancel()
	}, nil
}

// EmitTX publishes TX echo events, chunked to the registered MaxFrame.
// Uses the reader's pool for the device if available; otherwise falls back to a single copy and drop on pressure.
func (w *Worker) EmitTX(devID string, data []byte) {
	w.mu.RLock()
	rs := w.readers[devID]
	w.mu.RUnlock()
	if rs == nil || rs.pool == nil {
		// Fallback: one allocation, dropped if no space.
		p := append([]byte(nil), data...)
		ev := &Event{DevID: devID, Dir: "tx", Data: p, TS: time.Now()}
		select {
		case w.outQ <- ev:
		default:
		}
		return
	}

	max := rs.cfg.MaxFrame
	if max < 8 {
		max = 8
	}
	for off := 0; off < len(data); off += max {
		end := off + max
		if end > len(data) {
			end = len(data)
		}
		b := rs.pool.get()
		n := copy(b, data[off:end])
		ev := &Event{DevID: devID, Dir: "tx", Data: b[:n], TS: time.Now(), pool: rs.pool}
		select {
		case w.outQ <- ev:
		default:
			ev.Release() // drop under back-pressure
			return
		}
	}
}

// ---- tiny pooled slab allocator ----

type bufPool struct {
	free    chan []byte
	bufSize int
}

func newBufPool(n, size int) *bufPool {
	if n <= 0 {
		n = 1
	}
	if size <= 0 {
		size = 128
	}
	p := &bufPool{free: make(chan []byte, n), bufSize: size}
	for i := 0; i < n; i++ {
		p.free <- make([]byte, size)
	}
	return p
}

func (p *bufPool) get() []byte {
	return <-p.free
}

func (p *bufPool) put(b []byte) {
	if cap(b) != p.bufSize {
		return
	}
	b = b[:p.bufSize]
	select {
	case p.free <- b:
	default:
		// pool full; drop
	}
}

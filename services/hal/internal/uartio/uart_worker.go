// services/hal/internal/uartio/uart_worker.go
package uartio

import (
	"context"
	"time"

	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/util"
)

type Event struct {
	DevID string
	Dir   string // "rx" | "tx"
	Data  []byte
	TS    time.Time
}

type ReaderCfg struct {
	DevID         string
	Port          halcore.UARTPort
	Mode          string        // "bytes" | "lines"
	MaxFrame      int           // clamp 16..256
	IdleFlush     time.Duration // clamp 0..2s (lines mode)
	PublishTXEcho bool
}

type Worker struct {
	outQ chan Event
}

func New(outBuf int) *Worker {
	if outBuf <= 0 {
		outBuf = 64
	}
	return &Worker{outQ: make(chan Event, outBuf)}
}

func (w *Worker) Events() <-chan Event { return w.outQ }

// Register starts a bounded reader goroutine for a UART port. Returns cancel.
func (w *Worker) Register(ctx context.Context, cfg ReaderCfg) (func(), error) {
	max := cfg.MaxFrame
	if max < 16 {
		max = 16
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

	go func() {
		defer func() {
			// allow GC of buffers after exit
		}()
		buf := make([]byte, max)
		var line []byte

		timer := time.NewTimer(time.Hour)
		if !timer.Stop() {
			util.DrainTimer(timer)
		}

		flush := func(now time.Time) {
			if len(line) == 0 {
				return
			}
			payload := append([]byte(nil), line...)
			line = line[:0]
			select {
			case w.outQ <- Event{DevID: cfg.DevID, Dir: "rx", Data: payload, TS: now}:
			default:
				// drop if consumer is slow
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
				return
			case <-cfg.Port.Readable():
				// Bound the blocking wait to assist shutdown.
				rctx, rcancel := context.WithTimeout(cctx, 250*time.Millisecond)
				n, _ := cfg.Port.RecvSomeContext(rctx, buf)
				rcancel()
				if n <= 0 {
					continue
				}
				now := time.Now()
				if cfg.Mode == "lines" {
					// Accumulate UTF-8-ish lines; ignore CR; split on LF.
					for i := 0; i < n; i++ {
						b := buf[i]
						switch b {
						case '\n':
							flush(now)
						case '\r':
							// ignore
						default:
							if len(line) < max {
								line = append(line, b)
							}
						}
					}
				} else {
					// Emit raw chunk (binary-safe).
					payload := append([]byte(nil), buf[:n]...)
					select {
					case w.outQ <- Event{DevID: cfg.DevID, Dir: "rx", Data: payload, TS: now}:
					default:
					}
				}
			case <-timer.C:
				flush(time.Now())
			}
		}
	}()

	return cancel, nil
}

// EmitTX optionally publishes a TX echo event.
func (w *Worker) EmitTX(devID string, data []byte) {
	p := append([]byte(nil), data...)
	select {
	case w.outQ <- Event{DevID: devID, Dir: "tx", Data: p, TS: time.Now()}:
	default:
	}
}

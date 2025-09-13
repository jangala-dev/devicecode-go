// services/hal/worker.go
package hal

import (
	"context"
	"time"
)

type measureWorker struct {
	cfg  WorkerConfig
	reqQ chan MeasureReq
	sink chan<- Result

	pending  map[string]*collectItem
	want     map[string]bool
	collects []*collectItem
	timer    *time.Timer
}

type collectItem struct {
	id      string
	adaptor Adaptor
	due     time.Time
	retries int
}

func NewWorker(cfg WorkerConfig, sink chan<- Result) *measureWorker {
	if cfg.TriggerTimeout <= 0 {
		cfg.TriggerTimeout = 100 * time.Millisecond
	}
	if cfg.CollectTimeout <= 0 {
		cfg.CollectTimeout = 250 * time.Millisecond
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 15 * time.Millisecond
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 6
	}
	if cfg.InputQueueSize <= 0 {
		cfg.InputQueueSize = 16
	}
	if cfg.ResultsQueueSz <= 0 {
		cfg.ResultsQueueSz = 16
	}
	return &measureWorker{
		cfg: cfg, reqQ: make(chan MeasureReq, cfg.InputQueueSize),
		sink:    sink,
		pending: map[string]*collectItem{},
		want:    map[string]bool{},
		timer:   time.NewTimer(time.Hour),
	}
}

func (w *measureWorker) Submit(req MeasureReq) bool {
	select {
	case w.reqQ <- req:
		return true
	default:
		if req.Prio {
			select {
			case w.reqQ <- req:
				return true
			case <-time.After(5 * time.Millisecond):
			}
		}
		return false
	}
}

func (w *measureWorker) Start(ctx context.Context) {
	if !w.timer.Stop() {
		drainTimer(w.timer)
	}
	go func() {
		for {
			next := w.minDue()
			if next.IsZero() {
				if !w.timer.Stop() {
					drainTimer(w.timer)
				}
				w.timer.Reset(time.Hour)
			} else {
				d := time.Until(next)
				if d < 0 {
					d = 0
				}
				if !w.timer.Stop() {
					drainTimer(w.timer)
				}
				w.timer.Reset(d)
			}
			select {
			case <-ctx.Done():
				return
			case req := <-w.reqQ:
				if _, ok := w.pending[req.ID]; ok {
					if req.Prio {
						w.want[req.ID] = true
					}
					continue
				}
				tctx, cancel := context.WithTimeout(ctx, w.cfg.TriggerTimeout)
				after, err := req.Adaptor.Trigger(tctx)
				cancel()
				if err != nil {
					w.emit(Result{ID: req.ID, Err: err})
					continue
				}
				it := &collectItem{id: req.ID, adaptor: req.Adaptor, due: time.Now().Add(after)}
				w.pending[req.ID] = it
				w.collects = append(w.collects, it)
			case <-w.timer.C:
				now := time.Now()
				var keep []*collectItem
				for _, it := range w.collects {
					if now.Before(it.due) {
						keep = append(keep, it)
						continue
					}
					cctx, cancel := context.WithTimeout(ctx, w.cfg.CollectTimeout)
					s, err := it.adaptor.Collect(cctx)
					cancel()
					switch {
					case err == nil:
						delete(w.pending, it.id)
						delete(w.want, it.id)
						w.emit(Result{ID: it.id, Sample: s})
					case err == ErrNotReady && it.retries < w.cfg.MaxRetries:
						it.retries++
						it.due = now.Add(w.cfg.RetryBackoff)
						keep = append(keep, it)
					default:
						delete(w.pending, it.id)
						w.emit(Result{ID: it.id, Err: err})
						if w.want[it.id] {
							tctx, cancel := context.WithTimeout(ctx, w.cfg.TriggerTimeout)
							after, terr := it.adaptor.Trigger(tctx)
							cancel()
							if terr == nil {
								it.retries = 0
								it.due = time.Now().Add(after)
								w.pending[it.id] = it
								keep = append(keep, it)
							}
							delete(w.want, it.id)
						}
					}
				}
				w.collects = keep
			}
		}
	}()
}

func (w *measureWorker) emit(r Result) {
	select {
	case w.sink <- r:
	default:
		w.sink <- r
	}
}

func (w *measureWorker) minDue() time.Time {
	var min time.Time
	for _, it := range w.collects {
		if min.IsZero() || it.due.Before(min) {
			min = it.due
		}
	}
	return min
}

func drainTimer(t *time.Timer) {
	select {
	case <-t.C:
	default:
	}
}

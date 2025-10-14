package core

import (
	"container/heap"
	"context"
	"devicecode-go/types"
	"math/rand"
	"sync"
	"time"
)

type PollReq struct {
	Domain string
	Kind   types.Kind
	Name   string
	Verb   string
	Every  time.Duration
}

type pollKey struct {
	d    string
	k    types.Kind
	n    string
	verb string
}

type pollItem struct {
	key    pollKey
	due    int64
	every  time.Duration
	jitter time.Duration
	index  int
}

type pollHeap []*pollItem

func (h pollHeap) Len() int           { return len(h) }
func (h pollHeap) Less(i, j int) bool { return h[i].due < h[j].due }
func (h pollHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *pollHeap) Push(x any)        { it := x.(*pollItem); it.index = len(*h); *h = append(*h, it) }
func (h *pollHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	it.index = -1
	*h = old[:n-1]
	return it
}
func (h pollHeap) Top() *pollItem {
	if len(h) == 0 {
		return nil
	}
	return h[0]
}

type Poller struct {
	mu    sync.Mutex
	wake  chan struct{}
	items map[pollKey]*pollItem
	h     pollHeap
	rand  *rand.Rand
	out   chan<- PollReq
}

func NewPoller(out chan<- PollReq) *Poller {
	return &Poller{
		wake:  make(chan struct{}, 1),
		items: make(map[pollKey]*pollItem),
		rand:  rand.New(rand.NewSource(time.Now().UnixNano())),
		out:   out,
	}
}

// Upsert adds or updates a schedule.
// The first fire occurs after interval plus a random jitter in [0..jitter].
// Jitter is also applied on each subsequent re-arm.
func (p *Poller) Upsert(d string, k types.Kind, n, verb string, interval, jitter time.Duration) {
	if interval <= 0 || verb == "" {
		return
	}
	key := pollKey{d: d, k: k, n: n, verb: verb}

	p.mu.Lock()
	if jitter < 0 {
		jitter = 0
	}
	nextDue := time.Now().Add(p.jittered(interval, jitter)).UnixNano()
	if it := p.items[key]; it == nil {
		// Insert path: populate fully then Push (no Fix).
		it2 := &pollItem{
			key:    key,
			due:    nextDue,
			every:  interval,
			jitter: jitter,
			index:  -1,
		}
		p.items[key] = it2
		heap.Push(&p.h, it2)
	} else {
		// Update path: mutate + Fix.
		it.every = interval
		it.jitter = jitter
		it.due = nextDue
		heap.Fix(&p.h, it.index)
	}
	p.mu.Unlock()
	p.wakeup()
}

func (p *Poller) Stop(d string, k types.Kind, n, verb string) {
	key := pollKey{d: d, k: k, n: n, verb: verb}
	p.mu.Lock()
	if it := p.items[key]; it != nil {
		heap.Remove(&p.h, it.index)
		delete(p.items, key)
	}
	p.mu.Unlock()
	p.wakeup()
}

func (p *Poller) BumpAfter(d string, k types.Kind, n, verb string, lastEmitNs int64) {
	key := pollKey{d: d, k: k, n: n, verb: verb}
	now := time.Now()
	p.mu.Lock()
	if it := p.items[key]; it != nil {
		due := time.Unix(0, lastEmitNs).Add(it.every)
		if due.Before(now) {
			due = now
		}
		it.due = due.UnixNano()
		heap.Fix(&p.h, it.index)
	}
	p.mu.Unlock()
	p.wakeup()
}

func (p *Poller) Run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		wait := p.nextWait()
		if wait < 0 {
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
				continue
			}
		}
		if wait == 0 {
			var fire *pollItem

			p.mu.Lock()
			now := time.Now().UnixNano()
			top := p.h.Top()
			if top != nil && top.due <= now {
				fire = heap.Pop(&p.h).(*pollItem)
				fire.due = time.Now().Add(p.jittered(fire.every, fire.jitter)).UnixNano()
				heap.Push(&p.h, fire)
			}
			p.mu.Unlock()

			if fire != nil {
				select {
				case p.out <- PollReq{
					Domain: fire.key.d,
					Kind:   fire.key.k,
					Name:   fire.key.n,
					Verb:   fire.key.verb,
					Every:  fire.every,
				}:
				default:
				}
			}
			continue
		}

		timer.Reset(time.Duration(wait))
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (p *Poller) nextWait() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	top := p.h.Top()
	if top == nil {
		return -1
	}
	now := time.Now().UnixNano()
	if top.due <= now {
		return 0
	}
	return top.due - now
}

func (p *Poller) wakeup() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Poller) jittered(interval, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return interval
	}
	extra := time.Duration(p.rand.Int63n(int64(jitter) + 1)) // [0..jitter]
	return interval + extra
}

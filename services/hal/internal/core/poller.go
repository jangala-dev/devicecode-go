package core

import (
	"container/heap"
	"devicecode-go/types"
	"time"
)

// ---------------- Inlined poller: types & helpers ----------------

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

func (h *HAL) pollUpsert(d string, k types.Kind, n, verb string, interval, jitter time.Duration) {
	if interval <= 0 || verb == "" {
		return
	}
	key := pollKey{d: d, k: k, n: n, verb: verb}
	nextDue := time.Now().Add(h.jittered(interval, jitter)).UnixNano()
	if it := h.pollItems[key]; it == nil {
		it2 := &pollItem{
			key:    key,
			due:    nextDue,
			every:  interval,
			jitter: jitter,
			index:  -1,
		}
		h.pollItems[key] = it2
		heap.Push(&h.pollHeap, it2)
	} else {
		it.every = interval
		it.jitter = jitter
		it.due = nextDue
		heap.Fix(&h.pollHeap, it.index)
	}
	h.pollReschedule()
}

func (h *HAL) pollStop(d string, k types.Kind, n, verb string) {
	key := pollKey{d: d, k: k, n: n, verb: verb}
	if it := h.pollItems[key]; it != nil {
		heap.Remove(&h.pollHeap, it.index)
		delete(h.pollItems, key)
		h.pollReschedule()
	}
}

func (h *HAL) pollBumpAfter(d string, k types.Kind, n, verb string, lastEmitNs int64) {
	key := pollKey{d: d, k: k, n: n, verb: verb}
	if it := h.pollItems[key]; it != nil {
		due := time.Unix(0, lastEmitNs).Add(it.every)
		if due.Before(time.Now()) {
			due = time.Now()
		}
		it.due = due.UnixNano()
		heap.Fix(&h.pollHeap, it.index)
		h.pollReschedule()
	}
}

func (h *HAL) pollReschedule() {
	select {
	case h.pollWake <- struct{}{}:
	default:
	}
}

func (h *HAL) pollNextWait() time.Duration {
	top := h.pollHeap.Top()
	if top == nil {
		return -1
	}
	now := time.Now().UnixNano()
	if top.due <= now {
		return 0
	}
	return time.Duration(top.due - now)
}

func (h *HAL) pollFireDue() *pollItem {
	now := time.Now().UnixNano()
	top := h.pollHeap.Top()
	if top != nil && top.due <= now {
		fire := heap.Pop(&h.pollHeap).(*pollItem)
		fire.due = time.Now().Add(h.jittered(fire.every, fire.jitter)).UnixNano()
		heap.Push(&h.pollHeap, fire)
		return fire
	}
	return nil
}

func (h *HAL) jittered(interval, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return interval
	}
	extra := time.Duration(h.randJitter.Int63n(int64(jitter) + 1)) // [0..jitter]
	return interval + extra
}

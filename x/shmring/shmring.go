package shmring

import (
	"sync"
	"sync/atomic"
)

type Handle uint32

// global registry
var (
	regMu   sync.RWMutex
	rings          = map[Handle]*Ring{}
	nextHdl Handle = 1
)

func New(size int) (Handle, *Ring) {
	if size < 2 || (size&(size-1)) != 0 {
		panic("shmring: size must be power of two >= 2")
	}
	r := &Ring{
		buf:      make([]byte, size),
		mask:     uint32(size - 1),
		readable: make(chan struct{}, 1),
		writable: make(chan struct{}, 1),
	}
	regMu.Lock()
	h := nextHdl
	nextHdl++
	rings[h] = r
	regMu.Unlock()
	return h, r
}

func Get(h Handle) *Ring {
	if h == 0 {
		return nil
	}
	regMu.RLock()
	r := rings[h]
	regMu.RUnlock()
	return r
}

func Close(h Handle) {
	regMu.Lock()
	delete(rings, h)
	regMu.Unlock()
}

// Ring is a single-producer, single-consumer byte ring.
type Ring struct {
	buf  []byte
	mask uint32
	rd   atomic.Uint32 // consumer index (monotonic)
	wr   atomic.Uint32 // producer index (monotonic)

	readable chan struct{} // 0->>0 available edge
	writable chan struct{} // 0->>0 space edge
}

// Producer side
func (r *Ring) size() uint32 { return uint32(len(r.buf)) }

func (r *Ring) Space() int {
	rd := r.rd.Load()
	wr := r.wr.Load()
	return int(r.size() - (wr - rd))
}

func (r *Ring) Available() int {
	rd := r.rd.Load()
	wr := r.wr.Load()
	return int(wr - rd)
}

func (r *Ring) WriteFrom(src []byte) (n int) {
	if len(src) == 0 {
		return 0
	}
	rd := r.rd.Load()
	wr := r.wr.Load()
	beforeAvail := wr - rd
	space := int(r.size() - beforeAvail)
	if space <= 0 {
		return 0
	}
	if len(src) < space {
		space = len(src)
	}
	n = space

	size := r.size()
	wrIdx := wr & r.mask
	first := int(size - wrIdx)
	if first > n {
		first = n
	}
	copy(r.buf[wrIdx:wrIdx+uint32(first)], src[:first])
	if second := n - first; second > 0 {
		copy(r.buf[:second], src[first:n])
	}
	r.wr.Store(wr + uint32(n)) // release

	// Notify reader if we transitioned 0->>0 available
	if beforeAvail == 0 {
		select {
		case r.readable <- struct{}{}:
		default:
		}
	}
	return n
}

// Consumer side

func (r *Ring) ReadInto(dst []byte) (n int) {
	if len(dst) == 0 {
		return 0
	}
	rd := r.rd.Load()
	wr := r.wr.Load() // acquire
	avail := int(wr - rd)
	if avail <= 0 {
		return 0
	}
	if len(dst) < avail {
		avail = len(dst)
	}
	n = avail

	size := r.size()
	rdIdx := rd & r.mask
	first := int(size - rdIdx)
	if first > n {
		first = n
	}
	copy(dst[:first], r.buf[rdIdx:rdIdx+uint32(first)])
	if second := n - first; second > 0 {
		copy(dst[first:n], r.buf[:second])
	}
	r.rd.Store(rd + uint32(n)) // release

	// Notify writer if we transitioned 0->>0 space
	beforeSpace := int(size - (wr - rd))
	if beforeSpace == 0 {
		select {
		case r.writable <- struct{}{}:
		default:
		}
	}
	return n
}

func (r *Ring) Watermarks() (rd, wr uint32) {
	return r.rd.Load(), r.wr.Load()
}

func (r *Ring) Readable() <-chan struct{} { return r.readable }
func (r *Ring) Writable() <-chan struct{} { return r.writable }

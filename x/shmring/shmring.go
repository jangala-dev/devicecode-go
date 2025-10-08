// Package shmring provides a single-producer / single-consumer (SPSC) byte ring
// with edge-coalesced readiness channels and zero-copy span APIs.
//
// Semantics
//   - Exactly one producer goroutine and exactly one consumer goroutine.
//   - Capacity must be a power of two >= 2.
//   - Indices are uint32 and may wrap; distances use modular arithmetic.
//   - Distance invariant: 0 ≤ (wr - rd) ≤ size at all times.
//   - Empty: wr == rd. Full: (wr - rd) == size.
//   - Readiness notifications are edge-coalesced (buffered size 1); always
//     re-check state after waking.
//
// APIs
//   - Spans:    WriteAcquire/WriteCommit, ReadAcquire/ReadRelease
//   - Helpers:  TryWriteFrom, TryReadInto (copy-based)
//   - Introspection: Available(), Space(), Cap(), Readable(), Writable()
package shmring

import (
	"sync/atomic"
)

type Ring struct {
	buf  []byte
	mask uint32
	rd   atomic.Uint32 // consumer index (monotonic modulo size)
	wr   atomic.Uint32 // producer index (monotonic modulo size)

	readable chan struct{} // empty -> non-empty edge
	writable chan struct{} // full  -> non-full  edge
}

// New returns a ring with the given power-of-two size (>= 2).
func New(size int) *Ring {
	if size < 2 || size&(size-1) != 0 {
		panic("shmring: size must be power of two >= 2")
	}
	return &Ring{
		buf:      make([]byte, size),
		mask:     uint32(size - 1),
		readable: make(chan struct{}, 1),
		writable: make(chan struct{}, 1),
	}
}

func (r *Ring) size() uint32 { return uint32(len(r.buf)) }

// Cap returns the capacity in bytes.
func (r *Ring) Cap() int { return len(r.buf) }

// Available returns bytes available to the consumer.
func (r *Ring) Available() int {
	rd := r.rd.Load()
	wr := r.wr.Load()
	return int(wr - rd)
}

// Space returns bytes free for the producer.
func (r *Ring) Space() int {
	rd := r.rd.Load()
	wr := r.wr.Load()
	return int(r.size() - (wr - rd))
}

// Readable returns a coalesced notification when the ring transitions
// from empty to non-empty. Always re-check state after wake.
func (r *Ring) Readable() <-chan struct{} { return r.readable }

// Writable returns a coalesced notification when the ring transitions
// from full to non-full. Always re-check state after wake.
func (r *Ring) Writable() <-chan struct{} { return r.writable }

// ---- Span API ----

// WriteAcquire returns up to two contiguous writable spans (p1, p2).
// The producer must call WriteCommit(n) to publish written bytes.
func (r *Ring) WriteAcquire() (p1, p2 []byte) {
	rd := r.rd.Load()
	wr := r.wr.Load()
	size := r.size()

	space := size - (wr - rd)
	if space == 0 {
		return nil, nil
	}
	wrIdx := wr & r.mask
	first := int(size - wrIdx)
	if uint32(first) > space {
		first = int(space)
	}
	p1 = r.buf[wrIdx : wrIdx+uint32(first)]
	rem := int(space) - first
	if rem > 0 {
		p2 = r.buf[:rem]
	}
	return p1, p2
}

// WriteCommit publishes n bytes previously reserved by WriteAcquire.
// n must be in [0, len(p1)+len(p2)] of the last WriteAcquire call.
func (r *Ring) WriteCommit(n int) {
	if n <= 0 {
		return
	}
	rd := r.rd.Load()
	wr := r.wr.Load()
	beforeAvail := wr - rd

	r.wr.Store(wr + uint32(n)) // release to consumer

	// Notify consumer on empty->non-empty transition.
	if beforeAvail == 0 {
		select {
		case r.readable <- struct{}{}:
		default:
		}
	}
}

// ReadAcquire returns up to two contiguous readable spans (p1, p2).
// The consumer must call ReadRelease(n) to advance the consumer index.
func (r *Ring) ReadAcquire() (p1, p2 []byte) {
	rd := r.rd.Load()
	wr := r.wr.Load()
	size := r.size()

	avail := wr - rd
	if avail == 0 {
		return nil, nil
	}
	rdIdx := rd & r.mask
	first := int(size - rdIdx)
	if uint32(first) > avail {
		first = int(avail)
	}
	p1 = r.buf[rdIdx : rdIdx+uint32(first)]
	rem := int(avail) - first
	if rem > 0 {
		p2 = r.buf[:rem]
	}
	return p1, p2
}

// ReadRelease consumes n bytes previously obtained by ReadAcquire.
// n must be in [0, len(p1)+len(p2)] of the last ReadAcquire call.
func (r *Ring) ReadRelease(n int) {
	if n <= 0 {
		return
	}
	rd := r.rd.Load()
	wr := r.wr.Load()
	size := r.size()
	beforeSpace := size - (wr - rd)

	r.rd.Store(rd + uint32(n)) // release space to producer

	// Notify producer on full->non-full transition.
	if beforeSpace == 0 {
		select {
		case r.writable <- struct{}{}:
		default:
		}
	}
}

// ---- Copy helpers built on spans ----

// TryWriteFrom writes as much of src as fits now using spans.
// Returns bytes written (may be 0 if full).
func (r *Ring) TryWriteFrom(src []byte) int {
	if len(src) == 0 {
		return 0
	}
	p1, p2 := r.WriteAcquire()
	if len(p1) == 0 {
		return 0
	}
	n := copy(p1, src)
	if n < len(src) && len(p2) > 0 {
		n += copy(p2, src[n:])
	}
	r.WriteCommit(n)
	return n
}

// TryReadInto reads as much as available now into dst using spans.
// Returns bytes read (may be 0 if empty).
func (r *Ring) TryReadInto(dst []byte) int {
	if len(dst) == 0 {
		return 0
	}
	p1, p2 := r.ReadAcquire()
	if len(p1) == 0 {
		return 0
	}
	n := copy(dst, p1)
	if n < len(dst) && len(p2) > 0 {
		n += copy(dst[n:], p2)
	}
	r.ReadRelease(n)
	return n
}

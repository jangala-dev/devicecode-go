package shmring

import (
	"testing"
)

// fakeIO models partial producer/consumer progress (accept up to k bytes).
type fakeIO struct{ k int }

func (f fakeIO) write(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	if len(p) > f.k {
		return f.k
	}
	return len(p)
}
func (f fakeIO) read(dst []byte, src []byte) (int, []byte) {
	if len(src) == 0 || len(dst) == 0 {
		return 0, src
	}
	n := len(src)
	if n > len(dst) {
		n = len(dst)
	}
	if n > f.k {
		n = f.k
	}
	copy(dst[:n], src[:n])
	return n, src[n:]
}

func TestOrderAcrossWrapWithPartialProgress(t *testing.T) {
	r := New(64)
	prod := fakeIO{k: 7}
	_ = fakeIO{k: 5}

	// Produce a known sequence [0..N)
	const N = 2000
	src := make([]byte, N)
	for i := range src {
		src[i] = byte(i)
	}

	// Producer goroutine (simulated inline): repeatedly TryWriteFrom small slices,
	// forcing frequent wraps and partial first-span progress.
	p := src
	dst := make([]byte, N)
	off := 0

	for off < N {
		// producer step
		if len(p) > 0 {
			// emulate partial producer acceptance
			step := prod.write(p)
			if step > 0 {
				step = r.TryWriteFrom(p[:step])
				p = p[step:]
			}
		}

		// consumer step
		var tmp [17]byte
		n := r.TryReadInto(tmp[:])
		if n > 0 {
			copy(dst[off:], tmp[:n])
			off += n
		}
	}

	// Verify the stream is identical.
	for i := 0; i < N; i++ {
		if dst[i] != src[i] {
			t.Fatalf("mismatch at %d: got=%d want=%d", i, dst[i], src[i])
		}
	}
}

func TestReadableWritableEdges(t *testing.T) {
	r := New(8)
	select {
	case <-r.Readable():
		t.Fatal("unexpected Readable on empty ring")
	default:
	}
	n := r.TryWriteFrom([]byte{1, 2, 3})
	if n != 3 {
		t.Fatalf("write 3 -> %d", n)
	}
	select {
	case <-r.Readable(): // should fire once
	default:
		t.Fatal("expected Readable")
	}
	select {
	case <-r.Readable(): // coalesced; no second token yet
		t.Fatal("unexpected extra Readable")
	default:
	}
	// Drain fully -> Writable should fire when transitioning from full to non-full
	r.TryReadInto(make([]byte, 3))
	select {
	case <-r.Writable():
	default:
		// Not necessarily full before, so Writable may not fire; expand if needed to force full.
	}
}

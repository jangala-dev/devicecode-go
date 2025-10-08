# shmring

Single-producer / single-consumer (SPSC) byte ring for Go/TinyGo with:

- Edge-coalesced readiness notifications (`Readable`, `Writable`).
- Zero-copy spans (`WriteAcquire`/`WriteCommit`, `ReadAcquire`/`ReadRelease`).
- Copy helpers (`TryWriteFrom`, `TryReadInto`) built on spans.
- Optional handle registry to pass opaque identifiers between components.

The design assumes exactly one producer goroutine and exactly one consumer goroutine.

---

## Installation

```bash
go get github.com/you/yourrepo/shmring
````

---

## Quick start

```go
r := shmring.New(512) // capacity must be power of two >= 2

// Producer
go func() {
    msg := []byte("hello\n")
    off := 0
    for off < len(msg) {
        off += r.TryWriteFrom(msg[off:])
        if off < len(msg) {
            <-r.Writable() // coalesced; always re-check Space() after wake
        }
    }
}()

// Consumer
buf := make([]byte, 128)
for {
    n := r.TryReadInto(buf)
    if n == 0 {
        <-r.Readable() // coalesced; always re-check Available() after wake
        continue
    }
    os.Stdout.Write(buf[:n])
}
```

---

## Concurrency model and semantics

* **SPSC only:** exactly one producer goroutine and one consumer goroutine.
* **Capacity:** power of two, `>= 2`.
* **Indices:** `uint32` watermarks; differences use modular arithmetic.
* **Invariants:** the implementation maintains `0 ≤ (wr − rd) ≤ size` at all times.
* **Empty:** `wr == rd`. **Full:** `(wr − rd) == size`.
* **Notifications:** coalesced edges

  * `Readable`: fires on empty → non-empty.
  * `Writable`: fires on full → non-full.
    After any wake, **re-check** state.

---

## API reference

### Construction

```go
func New(size int) *Ring
```

Creates a ring of `size` bytes. `size` must be a power of two `>= 2`.

```go
type Handle uint32

func NewRegistered(size int) (Handle, *Ring)
func Register(r *Ring) Handle
func Get(h Handle) *Ring
func Close(h Handle)
```

Optional registry: obtain opaque handles for rings and look them up later. `Close` removes the handle mapping only; it does not alter the ring.

### Introspection

```go
func (r *Ring) Cap() int         // total capacity
func (r *Ring) Available() int   // bytes available to consumer (>= 0)
func (r *Ring) Space() int       // bytes free for producer   (>= 0)
func (r *Ring) Readable() <-chan struct{} // empty -> non-empty edge
func (r *Ring) Writable() <-chan struct{} // full  -> non-full  edge
```

### Zero-copy spans

Use spans when you want to avoid intermediate copying.

```go
func (r *Ring) WriteAcquire() (p1, p2 []byte)
func (r *Ring) WriteCommit(n int)

func (r *Ring) ReadAcquire() (p1, p2 []byte)
func (r *Ring) ReadRelease(n int)
```

Rules:

* Producer: call `WriteAcquire` to obtain up to two contiguous writable spans. Write into at most `len(p1)+len(p2)` bytes, then call `WriteCommit(n)` to publish exactly `n` bytes.
* Consumer: call `ReadAcquire` to obtain up to two contiguous readable spans. Read at most `len(p1)+len(p2)` bytes, then call `ReadRelease(n)` to consume exactly `n` bytes.
* **Ordering:** fill/drain `p1` to completion **before** using `p2` in the same pass. This preserves FIFO order at wrap points.
* **Do not modify** producer spans after `WriteCommit`. **Do not retain** consumer spans after `ReadRelease`.

### Copy helpers

Convenience shims built on spans. These perform a single copy per call and are non-blocking.

```go
func (r *Ring) TryWriteFrom(src []byte) int // returns bytes written (may be 0)
func (r *Ring) TryReadInto(dst []byte) int  // returns bytes read    (may be 0)
```

---

## Patterns

### Producer with pacing (copy helper)

```go
func writeAll(r *shmring.Ring, p []byte) {
    sent := 0
    for sent < len(p) {
        if n := r.TryWriteFrom(p[sent:]); n > 0 {
            sent += n
            continue
        }
        <-r.Writable()
    }
}
```

### Consumer with timeout (copy helper)

```go
func readSome(ctx context.Context, r *shmring.Ring, p []byte) (int, error) {
    if n := r.TryReadInto(p); n > 0 { return n, nil }
    for {
        select {
        case <-r.Readable():
            if n := r.TryReadInto(p); n > 0 { return n, nil }
        case <-ctx.Done():
            return 0, ctx.Err()
        }
    }
}
```

### Reactor bridge (spans)

The following sketch shows span use without intermediate staging. The “p1 before p2” rule is enforced in both directions.

```go
func bridge(ctx context.Context, dev Device, txRing, rxRing *shmring.Ring) {
    for {
        made := false

        // dev.Read -> rxRing
        for {
            p1, p2 := rxRing.WriteAcquire()
            if len(p1) == 0 { break }
            n1 := dev.TryRead(p1)
            if n1 == 0 { break }
            if n1 < len(p1) { rxRing.WriteCommit(n1); made = true; continue }
            n2 := 0
            if len(p2) > 0 { n2 = dev.TryRead(p2) }
            rxRing.WriteCommit(n1 + n2)
            made = true
        }

        // txRing -> dev.Write
        for {
            p1, p2 := txRing.ReadAcquire()
            if len(p1) == 0 { break }
            n1 := dev.TryWrite(p1)
            if n1 == 0 { break }
            if n1 < len(p1) { txRing.ReadRelease(n1); made = true; continue }
            n2 := 0
            if len(p2) > 0 { n2 = dev.TryWrite(p2) }
            txRing.ReadRelease(n1 + n2)
            made = true
        }

        if made { continue }
        select {
        case <-ctx.Done(): return
        case <-dev.Readable():
        case <-dev.Writable():
        case <-rxRing.Writable():
        case <-txRing.Readable():
        }
    }
}
```

`Device` here is any non-blocking source/sink with `TryRead`, `TryWrite`, `Readable`, `Writable`.

---

## Behavioural notes

* **Re-check after wake:** readiness channels are edge-coalesced. After any receive, re-test `Available()` or `Space()`.
* **No initial tokens:** `Writable` does not fire at start-up; the ring is not full. If you need to wait for space, first test `Space()`.
* **Fairness:** if one direction is dominant in a reactor, consider capping per-pass bytes to prevent starvation of the other direction.
* **Memory ordering:** the ring uses atomic loads/stores. The producer publishes data before advancing `wr`; the consumer reads `wr` before reading data; the consumer advances `rd` after finishing reads; the producer reads `rd` before writing. This is compatible with Go and TinyGo memory models.

---

## Registry usage

```go
// Create and register
hdl, r := shmring.NewRegistered(1024)

// Somewhere else
r2 := shmring.Get(hdl)
if r2 == nil { /* handle missing */ }

// Remove handle mapping (ring remains usable via r2)
shmring.Close(hdl)
```

---

## Limitations

* SPSC only. Do not use from multiple producers or multiple consumers.
* Capacity must be a power of two `>= 2`.
* Notifications are hints, not a level; callers must re-check state after wake.

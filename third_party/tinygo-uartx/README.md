# tinygo-uartx

Interrupt-driven UART for TinyGo with **blocking `io.Reader`/`io.Writer` semantics**, explicit non-blocking operations, and practical flush. Designed and tested on RP2040/RP2350 (PL011), with stubs for other TinyGo targets.

> **Compatibility notes**
> * `uartx` **breaks** with TinyGo’s `machine.UART` behaviour: **`Read(p)` blocks** until at least one byte is available. If you need non-blocking reads, use `TryRead`. See “API differences” below.
> * **TinyGo 0.39:** this currently requires the single-core scheduler. Build with `-scheduler tasks`.

---

## Why use `uartx`?

* **Correct, event-driven TX/RX**: uses hardware FIFOs and IRQs; foreground code does not poll during normal operation.
* **Clear Go semantics**: implements `io.Reader`, `io.Writer`, `io.ByteReader`, `io.ByteWriter` (via `ReadByte`/`WriteByte`) and a simple `Flusher` (`Flush()`).
* **Concurrent composability**: coalesced readiness channels `Readable()` and `Writable()` integrate naturally with `select` for fast, back-pressure-aware pacing.
* **On-the-wire completion**: `Flush()` waits for software buffer empty **and** hardware FIFO empty **and** the transmitter to go idle.
* **Production-oriented**: liveness at TX start, no foreground/ISR races, correct ordering of RX error handling, and minimal timed polling only where the hardware offers no interrupt (TX idle).

---

## Supported targets

* RP2040 / RP2350 (`go:build rp2040 || rp2350`) with PL011.
  Other arches are build-gated but not implemented here.

---

## Install

```bash
go get github.com/jangala-dev/tinygo-uartx/uartx
```

---

## Quick start (RP2040)

```go
package main

import (
	"machine"
	"time"

	"github.com/jangala-dev/tinygo-uartx/uartx"
)

func main() {
	u0 := uartx.UART0
	u1 := uartx.UART1

	// Wire: U0 TX=GP0 -> U1 RX=GP5, U1 TX=GP4 -> U0 RX=GP1
	_ = u0.Configure(uartx.UARTConfig{BaudRate: 230400, TX: machine.Pin(0), RX: machine.Pin(1)})
	_ = u1.Configure(uartx.UARTConfig{BaudRate: 230400, TX: machine.Pin(4), RX: machine.Pin(5)})

	// Writer: block until bytes are accepted by driver (SW TX ring and/or HW FIFO).
	msg := []byte("hello, world\n")
	_, _ = u0.Write(msg)

	// Reader: Read blocks until at least 1 byte is available, returns n>0, nil.
	buf := make([]byte, 64)
	n, _ := u1.Read(buf)
	_, _ = u1.Write(buf[:n]) // echo back

	// Ensure everything went on the wire.
	_ = u0.Flush()
	_ = u1.Flush()

	for { time.Sleep(time.Second) }
}
```

---

## API overview

### Blocking I/O

* `Read(p []byte) (int, error)`
  Blocks until **at least one byte** is available; returns `n>0, nil`. Does **not** return `io.EOF` for an idle UART.

* `Write(p []byte) (int, error)`
  Blocks until **all bytes are accepted** by the driver (software TX buffer and/or HW FIFO). Does **not** wait for the line to drain; see `Flush`.

* `ReadByte() (byte, error)`
  Non-blocking single-byte read from the software RX buffer. Returns `errUARTBufferEmpty` if no data is available.

* `WriteByte(b byte) error`
  Blocks until the byte is accepted by the driver.

### Non-blocking helpers

* `TryRead(p []byte) int`
  Returns immediately with up to `len(p)` bytes from the RX buffer. `0` means “no data now”.
* `TryWrite(p []byte) int`
  Returns immediately with `0..len(p)` bytes accepted into HW FIFO and/or SW TX buffer. `0` means “no space now”.

### Readiness (for `select`)

* `Readable() <-chan struct{}`
  Coalesced notification: a receive interrupt that enqueues ≥1 byte sends a token. You **must re-check** state after waking (level→edge coalescer).
* `Writable() <-chan struct{}`
  Coalesced notification: TX progress/space. Sent when bytes move SW→HW or space appears. Also level-coalesced; re-check state after waking.

### Flush

* `Flush() error`
  Blocks until software TX buffer is empty, the HW TX FIFO is empty, **and** the transmitter is not busy.
  Note: PL011 does not raise an interrupt for the final “idle” edge, so `Flush` uses a short timed poll (scaled to baud) in addition to readiness wakes.

### Buffer introspection

* `Buffered() int` – bytes in the RX ring.
* `TxFree() int` – free space in the SW TX ring.

### Interfaces satisfied

* `io.Reader`, `io.Writer`, `io.ByteReader`, `io.ByteWriter`
* `Flusher` (package-local `Flush() error`)

---

## API differences vs TinyGo `machine.UART`

| Behaviour              | `machine.UART` (TinyGo)  | `uartx`                                       |
| ---------------------- | ------------------------ | --------------------------------------------- |
| `Read(p)`              | **Non-blocking**         | **Blocking** until ≥1 byte                    |
| Non-blocking read      | `Read(p)`                | `TryRead(p) int`                              |
| Non-blocking write     | implementation-dependent | `TryWrite(p) int`                             |
| Event readiness        | varied                   | `Readable()`, `Writable()` coalesced channels |
| On-the-wire completion | `Write(p)`               | `Flush()` (FIFO empty **and** line idle)      |
| Internals              | polling/IRQ mix          | **IRQ-driven**; HW FIFOs; minimal timed poll  |

If you migrate from `machine.UART`, audit any paths that relied on `Read` being non-blocking. Use `TryRead` or `Readable()` with `select` for non-blocking behaviour.

---

## Concurrent patterns

### Producer with pacing

```go
func writeAll(u *uartx.UART, p []byte) {
	sent := 0
	for sent < len(p) {
		if n := u.TryWrite(p[sent:]); n > 0 {
			sent += n
			continue
		}
		<-u.Writable() // wait for space/progress; then re-check
	}
}
```

### Consumer with timeout

```go
func readSome(ctx context.Context, u *uartx.UART, p []byte) (int, error) {
	if n := u.TryRead(p); n > 0 { return n, nil }
	for {
		select {
		case <-u.Readable():
			if n := u.TryRead(p); n > 0 { return n, nil }
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}
```

### Duplex with `select`

```go
func pump(uIn, uOut *uartx.UART, buf []byte) {
	for {
		select {
		case <-uIn.Readable():
			if n := uIn.TryRead(buf); n > 0 {
				writeAll(uOut, buf[:n])
			}
		case <-uOut.Writable():
			// optional: send pending application data
		}
	}
}
```

---

## Behavioural notes (RP2040/RP2350)

* **Interrupt model**: RX uses level/timeout; TX uses level. Steady-state writes to the HW FIFO are performed in the ISR. Foreground only seeds the FIFO at TX start or in a guarded “masked kick” corner case; this avoids reordering.
* **Error handling**: framing/parity/overrun bytes are dropped on RX (read clears per-byte flags); sticky error status is cleared after draining.
* **Flush**: requires SW TX empty, FIFO empty and transmitter not busy. The final idle edge is not interrupt-driven on PL011; a short timed poll is used.

---

## Example: integrity test (excerpt)

```go
// Sender
func sendPattern(ctx context.Context, u *uartx.UART, gen func(int) byte, n int) error {
	var buf [192]byte
	for i := 0; i < n; {
		k := len(buf)
		if n-i < k { k = n - i }
		for j := 0; j < k; j++ { buf[j] = gen(i+j) }
		if _, err := sendAll(ctx, u, buf[:k]); err != nil { return err }
		i += k
	}
	return nil
}

func sendAll(ctx context.Context, u *uartx.UART, p []byte) (int, error) {
	sent := 0
	for sent < len(p) {
		if n := u.TryWrite(p[sent:]); n > 0 { sent += n; continue }
		select {
		case <-u.Writable():
		case <-ctx.Done(): return sent, ctx.Err()
		}
	}
	return sent, nil
}
```

---

## Limitations and future work

* Only RP2040/RP2350 implementation is included at present.
* RX overflow is dropped silently by default; add counters if required for diagnostics.
* CTS/RTS flow control is enabled only if both pins are configured; application-level tests advised.

---

## Licence

MIT, see `LICENCE` file.

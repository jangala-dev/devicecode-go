//go:build pico && pico_rich_dev

package main

import (
	"bytes"
	"context"
	"reflect"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/x/shmring"
)

func main() {
	println("[uart] boot …")
	time.Sleep(1500 * time.Millisecond)

	ctx := context.Background()
	b := bus.NewBus(4, "+", "#")
	halConn := b.NewConnection("hal")
	ui := b.NewConnection("ui")
	go hal.Run(ctx, halConn)
	time.Sleep(400 * time.Millisecond)

	// Topics
	tSessOpenU0 := bus.T("hal", "cap", "io", "serial", "uart0", "control", "session_open")
	tSessOpenU1 := bus.T("hal", "cap", "io", "serial", "uart1", "control", "session_open")
	tSessOpenedU0 := bus.T("hal", "cap", "io", "serial", "uart0", "event", "session_opened")
	tSessOpenedU1 := bus.T("hal", "cap", "io", "serial", "uart1", "event", "session_opened")
	tRxU1 := bus.T("hal", "cap", "io", "serial", "uart1", "event", "rx")
	tCloseU0 := bus.T("hal", "cap", "io", "serial", "uart0", "control", "session_close")
	tCloseU1 := bus.T("hal", "cap", "io", "serial", "uart1", "control", "session_close")

	// Subscriptions
	subU0Opened := ui.Subscribe(tSessOpenedU0)
	subU1Opened := ui.Subscribe(tSessOpenedU1)
	subU1Rx := ui.Subscribe(tRxU1)

	println("[uart] session_open uart0 …")
	if !reqOK(ui, tSessOpenU0, 2*time.Second) {
		println("[uart] FAIL: session_open(uart0) no reply")
		return
	}
	println("[uart] session_open uart1 …")
	if !reqOK(ui, tSessOpenU1, 2*time.Second) {
		println("[uart] FAIL: session_open(uart1) no reply")
		return
	}

	// Collect handles
	var u0tx, u1rx shmring.Handle
	deadline := time.Now().Add(3 * time.Second)
	for (u0tx == 0 || u1rx == 0) && time.Now().Before(deadline) {
		select {
		case m := <-subU0Opened.Channel():
			if h, ok := extractHandles(m.Payload); ok {
				u0tx = h.tx
				println("[uart] uart0 TX handle=", uint32(u0tx))
			}
		case m := <-subU1Opened.Channel():
			if h, ok := extractHandles(m.Payload); ok {
				u1rx = h.rx
				println("[uart] uart1 RX handle=", uint32(u1rx))
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if u0tx == 0 || u1rx == 0 {
		println("[uart] FAIL: missing session handles")
		return
	}

	tx0 := shmring.Get(u0tx)
	rx1 := shmring.Get(u1rx)
	if tx0 == nil || rx1 == nil {
		println("[uart] FAIL: ring lookup failed")
		return
	}

	// --- Smoke test ---
	println("[uart] smoke: send 'hello-uart' and verify")
	if !sendReceiveExact(tx0, rx1, subU1Rx.Channel(), []byte("hello-uart"), 3*time.Second) {
		println("[uart] smoke: FAIL")
	} else {
		println("[uart] smoke: PASS")
	}

	// --- Integrity test (FNV-1a over 4096 bytes, chunk 64) ---
	println("[uart] integrity: 4096 bytes, chunk 64")
	if integrityTest(tx0, rx1, subU1Rx.Channel(), 4096, 64, 5*time.Second) {
		println("[uart] integrity: PASS")
	} else {
		println("[uart] integrity: FAIL")
	}

	// --- Concurrent throughput test (writer + reader) ---
	println("[uart] throughput: 5s, chunk 256, concurrent R/W")
	thrConcurrent(tx0, rx1, 5*time.Second, 256, 256)

	// Best-effort close
	ui.Publish(ui.NewMessage(tCloseU0, nil, false))
	ui.Publish(ui.NewMessage(tCloseU1, nil, false))
	time.Sleep(100 * time.Millisecond)
}

// ---------------- helpers ----------------

type sess struct{ rx, tx shmring.Handle }

func extractHandles(v any) (sess, bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return sess{}, false
	}
	rxf := rv.FieldByName("RXHandle")
	txf := rv.FieldByName("TXHandle")
	if !(rxf.IsValid() && rxf.CanUint() && txf.IsValid() && txf.CanUint()) {
		return sess{}, false
	}
	return sess{rx: shmring.Handle(uint32(rxf.Uint())), tx: shmring.Handle(uint32(txf.Uint()))}, true
}

func reqOK(ui *bus.Connection, topic bus.Topic, to time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	_, err := ui.RequestWait(ctx, ui.NewMessage(topic, nil, false))
	return err == nil
}

// Smoke test: send msg and verify exact match.
func sendReceiveExact(tx *shmring.Ring, rx *shmring.Ring, rxEvents <-chan *bus.Message, msg []byte, timeout time.Duration) bool {
	_ = tx.WriteFrom(msg)
	deadline := time.Now().Add(timeout)

	buf := make([]byte, 0, 4*len(msg)) // small rolling window
	tmp := make([]byte, 128)

	drain := func() {
		for {
			n := rx.ReadInto(tmp)
			if n == 0 {
				return
			}
			buf = append(buf, tmp[:n]...)
			if len(buf) > cap(buf) {
				// keep only the tail
				copy(buf, buf[len(buf)-cap(buf):])
				buf = buf[:cap(buf)]
			}
		}
	}

	for time.Now().Before(deadline) {
		if i := bytes.Index(buf, msg); i >= 0 {
			return true
		}
		select {
		case <-rxEvents:
			drain()
		case <-time.After(25 * time.Millisecond):
			drain()
		}
	}
	println("[uart] smoke: not found; got bytes=", len(buf))
	return false
}

// Integrity test: send deterministic stream; compare FNV-1a hashes.
func integrityTest(tx *shmring.Ring, rx *shmring.Ring, rxEvents <-chan *bus.Message, totalBytes int, chunk int, timeout time.Duration) bool {
	gen := patternGenerator(0xA5)
	const off = uint32(2166136261)
	const prime = uint32(16777619)
	txHash, rxHash := off, off

	tmp := make([]byte, 128)
	deadline := time.Now().Add(timeout)

	out := make([]byte, chunk)
	written, received := 0, 0

	for (written < totalBytes || received < totalBytes) && time.Now().Before(deadline) {
		// write step
		if written < totalBytes {
			space := tx.Space()
			if space > 0 {
				if space > (totalBytes - written) {
					space = totalBytes - written
				}
				toWrite := chunk
				if toWrite > space {
					toWrite = space
				}
				fillPattern(out[:toWrite], &gen)
				n := tx.WriteFrom(out[:toWrite])
				if n > 0 {
					for i := 0; i < n; i++ {
						txHash ^= uint32(out[i])
						txHash *= prime
					}
					written += n
				}
			}
		}
		// read step
		for {
			n := rx.ReadInto(tmp)
			if n == 0 {
				break
			}
			for i := 0; i < n; i++ {
				rxHash ^= uint32(tmp[i])
				rxHash *= prime
			}
			received += n
			if received >= totalBytes {
				break
			}
		}
		// yield
		select {
		case <-rxEvents:
		case <-time.After(time.Millisecond):
		}
	}

	println("[uart] integrity: written=", written, " received=", received)
	println("[uart] integrity: txHash=", txHash, " rxHash=", rxHash)
	return written == totalBytes && received == totalBytes && txHash == rxHash
}

// Concurrent throughput test: writer + reader goroutines with shared stop.
func thrConcurrent(tx *shmring.Ring, rx *shmring.Ring, duration time.Duration, writeChunk, readBuf int) {
	if writeChunk <= 0 {
		writeChunk = 256
	}
	if readBuf <= 0 {
		readBuf = 256
	}

	out := make([]byte, writeChunk)
	in := make([]byte, readBuf)
	gen := patternGenerator(0x42)
	fillPattern(out, &gen)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	doneW := make(chan struct{})
	doneR := make(chan struct{})
	var written, received int

	// Writer
	go func() {
		defer close(doneW)
		for {
			for {
				space := tx.Space()
				if space <= 0 {
					break
				}
				if space > writeChunk {
					space = writeChunk
				}
				out[0] ^= gen.next()
				n := tx.WriteFrom(out[:space])
				if n == 0 {
					break
				}
				written += n
			}
			select {
			case <-tx.Writable():
			case <-ctx.Done():
				return
			}
		}
	}()

	// Reader
	go func() {
		defer close(doneR)
		for {
			for {
				n := rx.ReadInto(in)
				if n == 0 {
					break
				}
				received += n
			}
			select {
			case <-rx.Readable():
			case <-ctx.Done():
				// Grace drain
				grace := time.NewTimer(300 * time.Millisecond)
				defer grace.Stop()
				for {
					drained := false
					for {
						n := rx.ReadInto(in)
						if n == 0 {
							break
						}
						received += n
						drained = true
					}
					if !drained {
						select {
						case <-rx.Readable():
						case <-grace.C:
							return
						}
					}
				}
			}
		}
	}()

	<-doneW
	<-doneR

	elapsed := time.Since(start)
	if elapsed <= 0 {
		elapsed = time.Nanosecond
	}
	txBps := (int64(written) * int64(time.Second)) / int64(elapsed)
	rxBps := (int64(received) * int64(time.Second)) / int64(elapsed)

	println("[uart] throughput(concurrent): TX bytes=", written, " (~", txBps, " B/s)")
	println("[uart] throughput(concurrent): RX bytes=", received, " (~", rxBps, " B/s)")
}

// --- tiny utilities (no fmt) ---

// Simple deterministic pattern generator (xorshift8 over byte).
type patGen struct{ s byte }

func patternGenerator(seed byte) patGen { return patGen{s: seed} }
func (g *patGen) next() byte {
	x := g.s
	x ^= x << 3
	x ^= x >> 5
	x ^= x << 1
	g.s = x
	return x
}
func fillPattern(dst []byte, g *patGen) {
	for i := 0; i < len(dst); i++ {
		dst[i] = g.next()
	}
}

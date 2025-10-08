package main

import (
	"context"
	"math/rand"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"
	"devicecode-go/x/strconvx"

	"devicecode-go/x/shmring"
)

func printTopicWith(prefix string, t bus.Topic) {
	print(prefix)
	print(" ")
	for i := 0; i < t.Len(); i++ {
		if i > 0 {
			print("/")
		}
		switch v := t.At(i).(type) {
		case string:
			print(v)
		case int:
			print(v)
		case int32:
			print(int(v))
		case int64:
			print(int(v))
		default:
			print("?")
		}
	}
	println()
}

func openSerial(ctx context.Context, ui *bus.Connection, domain, name string, rxSize, txSize int) (types.SerialSessionOpened, error) {
	// Subscribe to the event that carries the session info.
	evT := bus.T("hal", "cap", domain, "serial", name, "event", "session_opened")
	sub := ui.Subscribe(evT)
	defer ui.Unsubscribe(sub)

	// Fire the enqueue-only control. Reply is just OK; real data comes via event.
	ctrlT := bus.T("hal", "cap", domain, "serial", name, "control", "session_open")
	printTopicWith("[test] will request on", ctrlT)
	if _, err := ui.RequestWait(ctx, ui.NewMessage(ctrlT, types.SerialSessionOpen{RXSize: rxSize, TXSize: txSize}, false)); err != nil {
		return types.SerialSessionOpened{}, err
	}

	// Wait for the event (use a bounded timeout derived from ctx).
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		select {
		case m := <-sub.Channel():
			if rep, ok := m.Payload.(types.SerialSessionOpened); ok {
				return rep, nil
			}
			// Keep waiting if some other payload slips in.
		case <-waitCtx.Done():
			return types.SerialSessionOpened{}, waitCtx.Err()
		}
	}
}

func main() {
	time.Sleep(3 * time.Second)
	println("[test] starting bus + HAL …")

	ctx := context.Background()
	b := bus.NewBus(4, "+", "#")
	halConn := b.NewConnection("hal")
	ui := b.NewConnection("ui")
	go hal.Run(ctx, halConn)

	time.Sleep(200 * time.Millisecond)

	// ---- Open uart0 ----
	println("[test] opening uart0 session …")
	open0, err := openSerial(ctx, ui, "io", "uart0", 512, 512)
	if err != nil {
		println("[test] uart0 open error:", err.Error())
		return
	}
	println("[test] uart0 opened, session id:", int(open0.SessionID))

	// ---- Open uart1 ----
	println("[test] opening uart1 session …")
	open1, err := openSerial(ctx, ui, "io", "uart1", 512, 512)
	if err != nil {
		println("[test] uart1 open error:", err.Error())
		return
	}
	println("[test] uart1 opened, session id:", int(open1.SessionID))

	// Resolve rings
	u0tx := shmring.Get(shmring.Handle(open0.TXHandle))
	u1rx := shmring.Get(shmring.Handle(open1.RXHandle))
	u1tx := shmring.Get(shmring.Handle(open1.TXHandle))
	u0rx := shmring.Get(shmring.Handle(open0.RXHandle))
	if u0tx == nil || u1rx == nil || u1tx == nil || u0rx == nil {
		println("[test] ring handle resolution failed")
		return
	}

	// ---- Sanity ping both ways (optional) ----
	println("[test] sending A from uart0 …")
	writeAll(u0tx, []byte("serial_raw reactor demo A: uart0 -> uart1\n"))
	drainPrint(u1rx)

	println("[test] sending B from uart1 …")
	writeAll(u1tx, []byte("serial_raw reactor demo B: uart1 -> uart0\n"))
	drainPrint(u0rx)

	// ---- Integrity test ----
	println("[test] integrity: uart0 -> uart1 (4KB, random chunking)")
	if !integrityTest(u0tx, u1rx) {
		println("[test] integrity FAILED")
		return
	}
	println("[test] integrity OK")

	// ---- Throughput tests ----
	println("[test] throughput: uart0 -> uart1 for 2s")
	bps01 := throughputTest(u0tx, u1rx, 2*time.Second)
	println("[test]  u0→u1 bytes/s:", bps01)

	println("[test] throughput: uart1 -> uart0 for 2s")
	bps10 := throughputTest(u1tx, u0rx, 2*time.Second)
	println("[test]  u1→u0 bytes/s:", bps10)

	// ---- Close sessions ----
	println("[test] closing sessions …")
	tU0Close := bus.T("hal", "cap", "io", "serial", "uart0", "control", "session_close")
	tU1Close := bus.T("hal", "cap", "io", "serial", "uart1", "control", "session_close")
	_, _ = ui.RequestWait(ctx, ui.NewMessage(tU0Close, types.SerialSessionClose{}, false))
	_, _ = ui.RequestWait(ctx, ui.NewMessage(tU1Close, types.SerialSessionClose{}, false))
	println("[test] done.")
}

// ---------------- helpers ----------------

func writeAll(r *shmring.Ring, p []byte) {
	written := 0
	for written < len(p) {
		if n := r.TryWriteFrom(p[written:]); n > 0 {
			written += n
			continue
		}
		<-r.Writable()
	}
}

func drainPrint(r *shmring.Ring) {
	var buf [256]byte
	t := time.NewTimer(50 * time.Millisecond)
	defer t.Stop()
	for {
		n := r.TryReadInto(buf[:])
		if n > 0 {
			print(string(buf[:n]))
			continue
		}
		select {
		case <-r.Readable():
		case <-t.C:
			return
		}
	}
}

func readSomeCtx(ctx context.Context, r *shmring.Ring, dst []byte) (int, error) {
	if n := r.TryReadInto(dst); n > 0 {
		return n, nil
	}
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-r.Readable():
			if n := r.TryReadInto(dst); n > 0 {
				return n, nil
			}
		}
	}
}

// ---------------- tests ----------------

func integrityTest(tx *shmring.Ring, rx *shmring.Ring) bool {
	const N = 4096
	var src [N]byte
	for i := 0; i < N; i++ {
		src[i] = byte(i & 0xFF)
	}

	done := make(chan struct{}, 1)
	go func() {
		rnd := rand.New(rand.NewSource(1))
		off := 0
		for off < N {
			ch := 1 + rnd.Intn(96)
			if off+ch > N {
				ch = N - off
			}
			writeAll(tx, src[off:off+ch])
			off += ch
			d := time.Duration(rnd.Intn(1500)) * time.Microsecond
			if d > 0 {
				time.Sleep(d)
			}
		}
		done <- struct{}{}
	}()

	exp := 0
	buf := make([]byte, 128)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for exp < N {
		n, err := readSomeCtx(ctx, rx, buf)
		if err != nil {
			println("[integrity] recv timeout")
			return false
		}
		for i := 0; i < n; i++ {
			want := byte((exp + i) & 0xFF)
			if buf[i] != want {
				print("[integrity] mismatch at ")
				print(strconvx.Itoa(exp + i))
				print(": got=")
				print(strconvx.Itoa(int(buf[i])))
				print(" want=")
				print(strconvx.Itoa(int(want)))
				println()
				panic("integrity mismatch")
			}
		}
		exp += n
	}
	<-done
	return true
}

func throughputTest(tx *shmring.Ring, rx *shmring.Ring, dur time.Duration) int {
	var blk [256]byte
	for i := 0; i < len(blk); i++ {
		blk[i] = byte((i*7 + 11) & 0xFF)
	}

	stop := time.Now().Add(dur)
	sentQuit := make(chan struct{}, 1)

	go func() {
		for time.Now().Before(stop) {
			if n := tx.TryWriteFrom(blk[:]); n == 0 {
				<-tx.Writable()
				continue
			}
		}
		sentQuit <- struct{}{}
	}()

	received := 0
	for time.Now().Before(stop) {
		if n := rx.TryReadInto(blk[:]); n > 0 {
			received += n
			continue
		}
		select {
		case <-rx.Readable():
		case <-time.After(500 * time.Microsecond):
		}
	}
	tEnd := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(tEnd) {
		if n := rx.TryReadInto(blk[:]); n > 0 {
			received += n
			continue
		}
		select {
		case <-rx.Readable():
		case <-time.After(500 * time.Microsecond):
		}
	}
	<-sentQuit
	return received / int(dur.Seconds())
}

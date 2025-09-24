package main

import (
	"context"
	"runtime"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"
)

// tiny helpers (no fmt)
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	sign := ""
	if i < 0 {
		sign = "-"
		i = -i
	}
	var buf [32]byte
	b := len(buf)
	for i > 0 {
		b--
		buf[b] = byte('0' + (i % 10))
		i /= 10
	}
	if sign != "" {
		b--
		buf[b] = '-'
	}
	return string(buf[b:])
}
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

// printMem prints a compact snapshot of TinyGo runtime memory stats.
func printMem() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	println(
		"[mem]",
		"alloc:", uint32(ms.Alloc),
		"heapInuse:", uint32(ms.HeapInuse),
		"heapSys:", uint32(ms.HeapSys),
		"mallocs:", uint32(ms.Mallocs),
		"frees:", uint32(ms.Frees),
	)
}

func main() {
	// Give the board a moment to settle (USB, clocks, etc.)
	time.Sleep(3 * time.Second)
	ctx := context.Background()

	println("[main] bootstrapping bus …")
	b := bus.NewBus(4)
	halConn := b.NewConnection("hal")
	uiConn := b.NewConnection("ui")

	println("[main] starting hal.Run …")
	// hal.Run publishes the compile-time setup (if any) before entering its loop.
	go hal.Run(ctx, halConn)

	// Allow time for HAL to apply the initial (compile-time) config and publish retained info/state.
	time.Sleep(250 * time.Millisecond)

	// Topics for led/0
	ledKind := string(types.KindLED)
	tValue := bus.T("hal", "capability", ledKind, 0, "value")
	tCtrlRead := bus.T("hal", "capability", ledKind, 0, "control", "read")
	tCtrlToggle := bus.T("hal", "capability", ledKind, 0, "control", "toggle")

	// Subscribe to value updates (authoritative data path)
	println("[main] subscribing to led/0 value …")
	valSub := uiConn.Subscribe(tValue)

	// Kick-off: request a read (reply is just ok/busy; value arrives on tValue)
	println("[main] requesting initial read of led/0 …")
	if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(tCtrlRead, nil, false)); err != nil {
		println("[main] read control request error:", err.Error())
	} else {
		printTopicWith("[main] read control reply on", reply.Topic)
	}

	println("[main] entering event loop (toggle every 500ms; print received values) …")

	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case m := <-valSub.Channel():
			// Expect types.LEDValue on payload
			switch v := m.Payload.(type) {
			case types.LEDValue:
				print("[value] led/0 level=")
				println(uint8(v.Level))
			case map[string]any:
				// Minimal fallback if routed via map (shouldn't happen with typed publishers)
				println("[value] led/0 (map payload)")
			default:
				println("[value] led/0 (unknown payload)")
			}
			_ = m

		case <-t.C:
			// Toggle the LED; control reply is immediate ok/busy. Actual level is observed via value subscription.
			if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(tCtrlToggle, nil, false)); err != nil {
				println("[main] toggle control error:", err.Error())
			} else {
				printTopicWith("[main] toggle control reply on", reply.Topic)
			}
			printMem()
		}
	}
}

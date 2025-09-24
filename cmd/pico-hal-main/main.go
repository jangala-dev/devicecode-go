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

// print fixed-point helpers without fmt

func printDeci(label string, deci int) {
	// deci: tenths (e.g. 231 => 23.1)
	sign := ""
	if deci < 0 {
		sign = "-"
		deci = -deci
	}
	whole := deci / 10
	frac := deci % 10
	print(label)
	print(sign)
	print(itoa(whole))
	print(".")
	print(itoa(frac))
	println()
}

func printHundredths(label string, hx100 int) {
	// hx100: hundredths (e.g. 5034 => 50.34)
	if hx100 < 0 {
		hx100 = 0
	}
	whole := hx100 / 100
	frac := hx100 % 100
	print(label)
	print(itoa(whole))
	print(".")
	if frac < 10 {
		print("0")
	}
	print(itoa(frac))
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

	// ---------- LED topics/subscriptions ----------
	ledKind := string(types.KindLED)
	tLEDValue := bus.T("hal", "capability", ledKind, 0, "value")
	tLEDCtrlRead := bus.T("hal", "capability", ledKind, 0, "control", "read")
	tLEDCtrlToggle := bus.T("hal", "capability", ledKind, 0, "control", "toggle")

	println("[main] subscribing to led/0 value …")
	ledSub := uiConn.Subscribe(tLEDValue)

	println("[main] requesting initial read of led/0 …")
	if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(tLEDCtrlRead, nil, false)); err != nil {
		println("[main] read control request error:", err.Error())
	} else {
		printTopicWith("[main] read control reply on", reply.Topic)
	}

	// ---------- SHTC3 topics/subscriptions ----------
	// Assumes exactly one temperature & humidity capability each (id 0),
	// as per pico_rich_dev setup with a single SHTC3 on i2c0.
	tempKind := string(types.KindTemperature)
	humidKind := string(types.KindHumidity)

	tTempValue := bus.T("hal", "capability", tempKind, 0, "value")
	tHumidValue := bus.T("hal", "capability", humidKind, 0, "value")
	tTempCtrlRead := bus.T("hal", "capability", tempKind, 0, "control", "read")

	println("[main] subscribing to temp/0 and humid/0 values …")
	tempSub := uiConn.Subscribe(tTempValue)
	humidSub := uiConn.Subscribe(tHumidValue)

	// Kick-off: request an initial sensor read (publishes both temp & humid values)
	println("[main] requesting initial read of temp/0 (shtc3) …")
	if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(tTempCtrlRead, nil, false)); err != nil {
		println("[main] temp read control request error:", err.Error())
	} else {
		printTopicWith("[main] temp read control reply on", reply.Topic)
	}

	println("[main] entering event loop (toggle LED every 500ms; read SHTC3 every 2s; print received values) …")

	// Use tickers to avoid per-loop timer allocations.
	ledTicker := time.NewTicker(500 * time.Millisecond)
	defer ledTicker.Stop()
	sensorTicker := time.NewTicker(2 * time.Second)
	defer sensorTicker.Stop()

	for {
		select {
		case m := <-ledSub.Channel():
			// Expect strictly typed types.LEDValue on payload
			if v, ok := m.Payload.(types.LEDValue); ok {
				print("[value] led/0 level=")
				println(uint8(v.Level))
			} else {
				println("[value] led/0 (unexpected payload type)")
			}

		case m := <-tempSub.Channel():
			if v, ok := m.Payload.(types.TemperatureValue); ok {
				printDeci("[value] temp/0 °C=", int(v.DeciC))
			} else {
				println("[value] temp/0 (unexpected payload type)")
			}

		case m := <-humidSub.Channel():
			if v, ok := m.Payload.(types.HumidityValue); ok {
				printHundredths("[value] humid/0 %RH=", int(v.RHx100))
			} else {
				println("[value] humid/0 (unexpected payload type)")
			}

		case <-ledTicker.C:
			// Toggle the LED; control reply is immediate ok/busy.
			if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(tLEDCtrlToggle, nil, false)); err != nil {
				println("[main] toggle control error:", err.Error())
			} else {
				printTopicWith("[main] toggle control reply on", reply.Topic)
			}
			// runtime.GC()
			printMem()

		case <-sensorTicker.C:
			// Request a sensor reading (publishes both temp & humid values).
			if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(tTempCtrlRead, nil, false)); err != nil {
				println("[main] temp read control error:", err.Error())
			} else {
				printTopicWith("[main] temp read control reply on", reply.Topic)
			}

		case <-ctx.Done():
			return
		}
	}
}

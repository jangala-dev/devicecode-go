package main

import (
	"context"
	"runtime"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"
	"devicecode-go/x/strconvx"
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

// fixed-point helpers (no fmt)

func printDeci(label string, deci int) {
	sign := ""
	if deci < 0 {
		sign = "-"
		deci = -deci
	}
	whole := deci / 10
	frac := deci % 10
	print(label)
	print(sign)
	print(strconvx.Itoa(whole))
	print(".")
	print(strconvx.Itoa(frac))
	println()
}

func printHundredths(label string, hx100 int) {
	if hx100 < 0 {
		hx100 = 0
	}
	whole := hx100 / 100
	frac := hx100 % 100
	print(label)
	print(strconvx.Itoa(whole))
	print(".")
	if frac < 10 {
		print("0")
	}
	print(strconvx.Itoa(frac))
	println()
}

// TinyGo runtime memory snapshot
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

func reqWaitFixed(ctx context.Context, c *bus.Connection, replyT bus.Topic, replySub *bus.Subscription, t bus.Topic, payload any) (*bus.Message, error) {
	msg := c.NewMessage(t, payload, false)
	msg.ReplyTo = replyT // avoid TNoIntern + subscribe
	c.Publish(msg)

	select {
	case m := <-replySub.Channel():
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func main() {
	// Allow board to settle (USB, clocks, etc.)
	time.Sleep(3 * time.Second)
	ctx := context.Background()

	println("[main] bootstrapping bus …")
	b := bus.NewBus(4, "+", "#")
	halConn := b.NewConnection("hal")
	uiConn := b.NewConnection("ui")

	replyTopic := bus.T("ui", "_rr") // fixed, interned
	replySub := uiConn.Subscribe(replyTopic)

	println("[main] starting hal.Run …")
	go hal.Run(ctx, halConn)

	// Allow HAL to publish initial retained state
	time.Sleep(250 * time.Millisecond)

	// ---------- PWM topics/subscriptions (onboard) ----------
	// Using hal/cap/<domain>/<kind>/<name>/...
	pwmKind := string(types.KindPWM) // ensure types.KindPWM exists
	tPWMValue := bus.T("hal", "cap", "io", pwmKind, "onboard", "value")
	tPWMCtrlSet := bus.T("hal", "cap", "io", pwmKind, "onboard", "control", "set")
	tPWMCtrlRamp := bus.T("hal", "cap", "io", pwmKind, "onboard", "control", "ramp")

	println("[main] subscribing to io/pwm/onboard value …")
	pwmSub := uiConn.Subscribe(tPWMValue)

	// Optional: set an initial level (0)
	println("[main] setting initial io/pwm/onboard level=0 …")
	if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(tPWMCtrlSet, types.PWMSet{Level: 0}, false)); err != nil {
		println("[main] pwm set control error:", err.Error())
	} else {
		printTopicWith("[main] pwm set control reply on", reply.Topic)
	}

	// ---------- SHTC3 topics/subscriptions ----------
	tempKind := string(types.KindTemperature)
	humidKind := string(types.KindHumidity)

	tTempValue := bus.T("hal", "cap", "env", tempKind, "core", "value")
	tHumidValue := bus.T("hal", "cap", "env", humidKind, "core", "value")
	tTempCtrlRead := bus.T("hal", "cap", "env", tempKind, "core", "control", "read")

	println("[main] subscribing to env/temperature/core and env/humidity/core values …")
	tempSub := uiConn.Subscribe(tTempValue)
	humidSub := uiConn.Subscribe(tHumidValue)

	println("[main] requesting initial read of env/temperature/core …")
	if reply, err := reqWaitFixed(ctx, uiConn, replyTopic, replySub, tTempCtrlRead, nil); err != nil {
		println("[main] temp read control request error:", err.Error())
	} else {
		printTopicWith("[main] temp read control reply on", reply.Topic)
	}

	println("[main] entering event loop (ramp PWM every 1s; read SHTC3 every 2s; print received values) …")

	// Tickers (no per-loop allocations)
	rampTicker := time.NewTicker(2 * time.Second)
	defer rampTicker.Stop()
	sensorTicker := time.NewTicker(2 * time.Second)
	defer sensorTicker.Stop()

	const pwmTop = 4095 // must match pwm_out Top
	levelUp := true

	for {
		select {
		case m := <-pwmSub.Channel():
			if v, ok := m.Payload.(types.PWMValue); ok {
				print("[value] io/pwm/onboard level=")
				println(uint16(v.Level))
			} else {
				println("[value] io/pwm/onboard (unexpected payload type)")
			}

		case m := <-tempSub.Channel():
			if v, ok := m.Payload.(types.TemperatureValue); ok {
				printDeci("[value] env/temperature/core °C=", int(v.DeciC))
			} else {
				println("[value] env/temperature/core (unexpected payload type)")
			}

		case m := <-humidSub.Channel():
			if v, ok := m.Payload.(types.HumidityValue); ok {
				printHundredths("[value] env/humidity/core %RH=", int(v.RHx100))
			} else {
				println("[value] env/humidity/core (unexpected payload type)")
			}

		case <-rampTicker.C:
			// Alternate ramp between 0 and Top over 1s in 32 steps (linear mode=0)
			var target uint16
			if levelUp {
				target = pwmTop
			} else {
				target = 0
			}
			levelUp = !levelUp

			payload := types.PWMRamp{To: target, DurationMs: 1000, Steps: 32, Mode: 0}
			if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(tPWMCtrlRamp, payload, false)); err != nil {
				println("[main] pwm ramp control error:", err.Error())
			} else {
				printTopicWith("[main] pwm ramp control reply on", reply.Topic)
			}
			runtime.GC()
			printMem()

		case <-sensorTicker.C:
			if reply, err := reqWaitFixed(ctx, uiConn, replyTopic, replySub, tTempCtrlRead, nil); err != nil {
				println("[main] temp read control error:", err.Error())
			} else {
				printTopicWith("[main] temp read control reply on", reply.Topic)
			}

		case <-ctx.Done():
			return
		}
	}
}

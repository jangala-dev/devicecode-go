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

func main() {
	// Allow board to settle (USB, clocks, etc.)
	time.Sleep(3 * time.Second)
	ctx := context.Background()

	println("[main] bootstrapping bus …")
	b := bus.NewBus(4, "+", "#")
	halConn := b.NewConnection("hal")
	uiConn := b.NewConnection("ui")

	println("[main] starting hal.Run …")
	go hal.Run(ctx, halConn)

	// Allow HAL to publish initial retained state
	time.Sleep(250 * time.Millisecond)

	// ---------- PWM topics/subscriptions (onboard) ----------
	// Using hal/cap/<domain>/<kind>/<name>/...
	pwmKind := string(types.KindPWM) // ensure types.KindPWM exists
	tPWMCtrlSet := bus.T("hal", "cap", "io", pwmKind, "button-led", "control", "set")
	tPWMCtrlRamp := bus.T("hal", "cap", "io", pwmKind, "button-led", "control", "ramp")

	// Optional: set an initial level (0)
	println("[main] setting initial io/pwm/onboard level=0 …")
	uiConn.Publish(uiConn.NewMessage(tPWMCtrlSet, types.PWMSet{Level: 0}, false))

	// ---------- SHTC3 topics/subscriptions ----------
	tempKind := string(types.KindTemperature)
	humidKind := string(types.KindHumidity)

	tTempValue := bus.T("hal", "cap", "env", tempKind, "core", "value")
	tHumidValue := bus.T("hal", "cap", "env", humidKind, "core", "value")

	println("[main] subscribing to env/temperature/core and env/humidity/core values …")
	tempSub := uiConn.Subscribe(tTempValue)
	humidSub := uiConn.Subscribe(tHumidValue)

	// Tickers (no per-loop allocations)
	rampTicker := time.NewTicker(2 * time.Second)
	defer rampTicker.Stop()

	const pwmTop = 4095 // must match pwm_out Top
	levelUp := true

	// ---------- SHTC3 topics/subscriptions ----------
	const (
		domain      = "power"
		batteryKind = string(types.KindBattery)
		chargerKind = string(types.KindCharger)
		name        = "internal"
	)

	println("[main] subscribing to power/charger/charger0 and env/battery/charger0 values …")
	valSub := uiConn.Subscribe(bus.T("hal", "cap", "power", "+", name, "value"))
	stSub := uiConn.Subscribe(bus.T("hal", "cap", "power", "+", name, "status"))
	evSub := uiConn.Subscribe(bus.T("hal", "cap", "power", "+", name, "event", "+"))

	valCh := valSub.Channel()
	stCh := stSub.Channel()
	evCh := evSub.Channel()

	// Track latest currents for ISYS≈IIN−IBAT
	var lastIIn int32
	var lastIBat int32
	var haveIIn, haveIBat bool

	println("[main] entering event loop (ramp PWM every 2s; print SHTC3 and LTC4015 print received values + events) …")

	for {
		select {

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
			uiConn.Publish(uiConn.NewMessage(tPWMCtrlRamp, payload, false))
			runtime.GC()
			printMem()

		case m, ok := <-valCh:
			if !ok {
				valCh = nil
				continue
			}
			printCapValue(m, &lastIIn, &haveIIn, &lastIBat, &haveIBat)

		case m, ok := <-stCh:
			if !ok {
				stCh = nil
				continue
			}
			printCapStatus(m)

		case m, ok := <-evCh:
			if !ok {
				evCh = nil
				continue
			}
			printCapEvent(m)

		case <-ctx.Done():
			return
		}
	}
}

// ----------- printing helpers -----------

func printCapValue(m *bus.Message, lastIIn *int32, haveIIn *bool, lastIBat *int32, haveIBat *bool) {
	// hal/cap/<domain>/<kind>/<name>/value
	dom, _ := m.Topic.At(2).(string)
	kind, _ := m.Topic.At(3).(string)
	name, _ := m.Topic.At(4).(string)

	switch v := m.Payload.(type) {
	case types.BatteryValue:
		print("[value] ")
		print(dom)
		print("/")
		print(kind)
		print("/")
		print(name)
		print(" | VBAT=")
		print(int(v.PackMilliV))
		print("mV per=")
		print(int(v.PerCellMilliV))
		print("mV | IBAT=")
		print(int(v.IBatMilliA))
		print("mA")

		*lastIBat = v.IBatMilliA
		*haveIBat = true

		// ISYS ≈ IIN − IBAT (IBAT>0 ⇒ charging)
		if *haveIIn && *haveIBat {
			isys := *lastIIn - *lastIBat
			print(" | ISYS≈")
			print(int(isys))
			print("mA")
		}

		println("")

	case types.ChargerValue:
		print("[value] ")
		print(dom)
		print("/")
		print(kind)
		print("/")
		print(name)
		print(" | VIN=")
		print(int(v.VIN_mV))
		print("mV | VSYS=")
		print(int(v.VSYS_mV))
		print("mV | IIN=")
		print(int(v.IIn_mA))
		print("mA")

		*lastIIn = v.IIn_mA
		*haveIIn = true

		if *haveIIn && *haveIBat {
			isys := *lastIIn - *lastIBat
			print(" | ISYS≈")
			print(int(isys))
			print("mA")
		}

		println("")
	default:
		// ignore others
	}
}

func printCapStatus(m *bus.Message) {
	// hal/cap/<domain>/<kind>/<name>/status
	dom, _ := m.Topic.At(2).(string)
	kind, _ := m.Topic.At(3).(string)
	name, _ := m.Topic.At(4).(string)

	// Battery/charger only
	if dom != "power" {
		return
	}
	if kind != string(types.KindBattery) && kind != string(types.KindCharger) {
		return
	}

	if s, ok := m.Payload.(types.CapabilityStatus); ok {
		print("[link] ")
		print(dom)
		print("/")
		print(kind)
		print("/")
		print(name)
		print(" | link=")
		print(string(s.Link))
		print(" ts=")
		println(s.TS)
	}
}

func printCapEvent(m *bus.Message) {
	// hal/cap/<domain>/<kind>/<name>/event/<tag>
	dom, _ := m.Topic.At(2).(string)
	kind, _ := m.Topic.At(3).(string)
	name, _ := m.Topic.At(4).(string)
	tag, _ := m.Topic.At(6).(string)

	if dom != "power" {
		return
	}
	if kind != string(types.KindBattery) && kind != string(types.KindCharger) {
		return
	}

	print("[event] ")
	print(dom)
	print("/")
	print(kind)
	print("/")
	print(name)
	print(" | ")
	println(tag)
}

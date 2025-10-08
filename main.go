package main

import (
	"context"
	"runtime"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"
	"devicecode-go/x/shmring"
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

// ---- fixed-point helpers (no fmt) ----

func itoa(i int) []byte { return []byte(strconvx.Itoa(i)) }

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

// ---- TinyGo runtime memory snapshot ----
func printMem() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	println(
		"[mem]",
		"alloc:", uint32(ms.Alloc),
		"heapSys:", uint32(ms.HeapSys),
		"mallocs:", uint32(ms.Mallocs),
		"frees:", uint32(ms.Frees),
	)
}

// ---- shmring helpers ----
var nl = [...]byte{'\n'}

func ringWriteAll(r *shmring.Ring, b []byte) {
	if r == nil || len(b) == 0 {
		return
	}
	_ = r.TryWriteFrom(b) // best-effort (non-blocking, may drop)
}
func ringWriteLine(r *shmring.Ring, b []byte) {
	if r == nil {
		return
	}
	if r.Space() >= len(b)+1 {
		_ = r.TryWriteFrom(b)
		_ = r.TryWriteFrom(nl[:])
	}
}

// ---- minimal logger (console + UART1) ----
var uart1Tx *shmring.Ring

func log_(b []byte) {
	// console
	print(string(b))
	// uart1
	ringWriteAll(uart1Tx, b)
}
func logln_(b []byte) {
	// console
	println(string(b))
	// uart1
	if uart1Tx != nil {
		_ = uart1Tx.TryWriteFrom(b)
		_ = uart1Tx.TryWriteFrom(nl[:])
	}
}

// ---- bus helpers ----
func reqOKTO(c *bus.Connection, t bus.Topic, payload any, to time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	_, err := c.RequestWait(ctx, c.NewMessage(t, payload, false))
	return err == nil
}

// ---- topics ----

// PWM
var (
	tPWMCtrlSet  = bus.T("hal", "cap", "io", string(types.KindPWM), "button-led", "control", "set")
	tPWMCtrlRamp = bus.T("hal", "cap", "io", string(types.KindPWM), "button-led", "control", "ramp")
)

// Env
var (
	tTempValue = bus.T("hal", "cap", "env", string(types.KindTemperature), "core", "value")
	tHumValue  = bus.T("hal", "cap", "env", string(types.KindHumidity), "core", "value")
)

// Power (subscribe wildcard kind for “internal”)
var (
	valTopic = bus.T("hal", "cap", "power", "+", "internal", "value")
	stTopic  = bus.T("hal", "cap", "power", "+", "internal", "status")
	evTopic  = bus.T("hal", "cap", "power", "+", "internal", "event", "+")
)

// Power switches
func tSwitch(name string) bus.Topic {
	return bus.T("hal", "cap", "power", string(types.KindSwitch), name, "control", "set")
}

var powerOrderUp = [...]string{"mpcie-usb", "m2", "mpcie", "cm5", "fan", "boost-load"}

// UART sessions
func tSessOpen(name string) bus.Topic {
	return bus.T("hal", "cap", "io", "serial", name, "control", "session_open")
}
func tSessOpened(name string) bus.Topic {
	return bus.T("hal", "cap", "io", "serial", name, "event", "session_opened")
}
func tSessClosed(name string) bus.Topic {
	return bus.T("hal", "cap", "io", "serial", name, "event", "session_closed")
}

// ---- telemetry JSON (flat, tiny, no fmt/json) ----
// {"t_deci":650,"vbat_mV":12400,"vin_mV":12000,"isys_mA":123}
func teleJSON(tdeci int, vbat, vin int32, isys int32) []byte {
	buf := []byte(`{"t_deci":`)
	buf = append(buf, itoa(tdeci)...)
	buf = append(buf, []byte(`,"vbat_mV":`)...)
	buf = append(buf, itoa(int(vbat))...)
	buf = append(buf, []byte(`,"vin_mV":`)...)
	buf = append(buf, itoa(int(vin))...)
	buf = append(buf, []byte(`,"isys_mA":`)...)
	buf = append(buf, itoa(int(isys))...)
	buf = append(buf, '}')
	return buf
}

func main() {
	// Allow board to settle (USB, clocks, etc.)
	time.Sleep(3 * time.Second)
	ctx := context.Background()

	logln_([]byte("[main] bootstrapping bus …"))
	b := bus.NewBus(2, "+", "#")
	halConn := b.NewConnection("hal")
	uiConn := b.NewConnection("ui")

	logln_([]byte("[main] starting hal.Run …"))
	go hal.Run(ctx, halConn)

	// Allow HAL to publish initial retained state
	time.Sleep(250 * time.Millisecond)

	// Set initial LED level (off)
	logln_([]byte("[main] set button-led=0"))
	uiConn.Publish(uiConn.NewMessage(tPWMCtrlSet, types.PWMSet{Level: 0}, false))

	// Subscriptions (env + power)
	logln_([]byte("[main] subscribing env + power …"))
	tempSub := uiConn.Subscribe(tTempValue)
	humidSub := uiConn.Subscribe(tHumValue)
	valSub := uiConn.Subscribe(valTopic)
	stSub := uiConn.Subscribe(stTopic)
	evSub := uiConn.Subscribe(evTopic)
	valCh := valSub.Channel()
	stCh := stSub.Channel()
	evCh := evSub.Channel()

	// UART sessions (TX only needed for our use)
	const (
		uartTele = "uart0" // telemetry JSON
		uartLog  = "uart1" // log mirror
	)
	subSessOpenTele := uiConn.Subscribe(tSessOpened(uartTele))
	subSessOpenLog := uiConn.Subscribe(tSessOpened(uartLog))
	subSessClosedTele := uiConn.Subscribe(tSessClosed(uartTele))
	subSessClosedLog := uiConn.Subscribe(tSessClosed(uartLog))

	// Kick open requests (fire-and-forget; events carry handles)
	uiConn.Publish(uiConn.NewMessage(tSessOpen(uartTele), nil, false))
	uiConn.Publish(uiConn.NewMessage(tSessOpen(uartLog), nil, false))

	// Switch sequencing helpers
	seqDown := func() {
		for i := len(powerOrderUp) - 1; i >= 0; i-- {
			uiConn.Publish(uiConn.NewMessage(tSwitch(powerOrderUp[i]), types.SwitchSet{On: false}, false))
			time.Sleep(200 * time.Millisecond)
		}
	}
	seqUp := func() {
		for _, name := range powerOrderUp {
			uiConn.Publish(uiConn.NewMessage(tSwitch(name), types.SwitchSet{On: true}, false))
			time.Sleep(200 * time.Millisecond)
		}
	}

	// State (PG + env + power + UART rings)
	const (
		pgOnVIN  = 12000 // mV
		pgOnVBAT = 12400 // mV
		debounce = 300 * time.Millisecond
		staleMax = 3 * time.Second
	)
	var (
		// UART TX rings
		uart0Tx *shmring.Ring // telemetry
		// uart1Tx declared global (logger)

		// env
		lastTDeci int
		tsTemp    int64

		// power
		lastVIN  int32
		lastVBAT int32
		tsVIN    int64
		tsVBAT   int64
		lastIIn  int32
		lastIBat int32
		haveIIn  bool
		haveIBat bool

		pgSince  time.Time
		pgStable bool
		railsUp  bool

		// LED behavior control
		ledSteady bool // true when we have applied steady-on for railsUp
	)

	// Single ticker does: LED control, mem stats, PG check, telemetry flush
	rampTicker := time.NewTicker(2 * time.Second)
	defer rampTicker.Stop()
	const pwmTop = 4095
	levelUp := true // for breathe mode only (railsDown)

	logln_([]byte("[main] entering loop (LED/mem/PG on one tick; env/power prints; UART0 telemetry; UART1 logs) …"))

	for {
		select {
		// ---- UART session opened/closed ----
		case m := <-subSessOpenTele.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				uart0Tx = shmring.Get(shmring.Handle(ev.TXHandle))
				logln_([]byte("[uart0] telemetry session opened"))
			}
		case m := <-subSessOpenLog.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				uart1Tx = shmring.Get(shmring.Handle(ev.TXHandle))
				logln_([]byte("[uart1] log session opened"))
			}
		case <-subSessClosedTele.Channel():
			uart0Tx = nil
			logln_([]byte("[uart0] telemetry session closed"))
		case <-subSessClosedLog.Channel():
			uart1Tx = nil
			logln_([]byte("[uart1] log session closed"))

		// ---- Env prints ----
		case m := <-tempSub.Channel():
			if v, ok := m.Payload.(types.TemperatureValue); ok {
				lastTDeci = int(v.DeciC)
				tsTemp = time.Now().UnixNano()
				printDeci("[value] env/temperature/core °C=", lastTDeci)
				// mirror to uart1
				ringWriteLine(uart1Tx, []byte("[value] temp °C="+strconvx.Itoa(lastTDeci/10)+"."+strconvx.Itoa(lastTDeci%10)))
			}

		case m := <-humidSub.Channel():
			if v, ok := m.Payload.(types.HumidityValue); ok {
				printHundredths("[value] env/humidity/core %RH=", int(v.RHx100))
				// mirror short log to uart1
				ringWriteLine(uart1Tx, []byte("[value] hum %RH="+strconvx.Itoa(int(v.RHx100/100))+"."+func() string {
					x := int(v.RHx100 % 100)
					if x < 10 {
						return "0" + strconvx.Itoa(x)
					}
					return strconvx.Itoa(x)
				}()))
			}

		// ---- LED + mem + PG + telemetry (one ticker) ----
		case <-rampTicker.C:
			// LED behavior tied to railsUp:
			// - railsUp => steady ON (send Set once when it flips up)
			// - railsDown => breathe (alternate ramp up/down each tick)
			if railsUp {
				if !ledSteady {
					uiConn.Publish(uiConn.NewMessage(tPWMCtrlSet, types.PWMSet{Level: pwmTop}, false))
					ledSteady = true
				}
			} else {
				// ensure we re-enter breathe when rails drop
				ledSteady = false
				var target uint16
				if levelUp {
					target = pwmTop
				} else {
					target = 0
				}
				levelUp = !levelUp
				uiConn.Publish(uiConn.NewMessage(tPWMCtrlRamp, types.PWMRamp{To: target, DurationMs: 1000, Steps: 32, Mode: 0}, false))
			}

			// Memory stats
			runtime.GC()
			printMem()

			// PG check (VIN or VBAT path). Debounce right here.
			now := time.Now()
			freshVIN := tsVIN != 0 && now.Sub(time.Unix(0, tsVIN)) <= staleMax
			freshBAT := tsVBAT != 0 && now.Sub(time.Unix(0, tsVBAT)) <= staleMax
			pgNow := (freshVIN && lastVIN >= pgOnVIN) || (freshBAT && lastVBAT >= pgOnVBAT)

			if pgNow {
				if pgSince.IsZero() {
					pgSince = now
				} else if !pgStable && now.Sub(pgSince) >= debounce {
					pgStable = true
					if !railsUp {
						logln_([]byte("[power] PG debounced → rails UP"))
						seqUp()
						railsUp = true
						// LED will switch to steady on next tick (handled above)
					}
				}
			} else {
				pgStable = false
				pgSince = time.Time{}
				if railsUp {
					logln_([]byte("[power] PG lost → rails DOWN"))
					seqDown()
					railsUp = false
					// LED will resume breathing next tick (handled above)
				}
			}

			// Telemetry over UART0 (flat JSON). Use freshest we have; ISYS≈IIN−IBAT when both known.
			if uart0Tx != nil && tsTemp != 0 {
				vin := int32(0)
				vbat := int32(0)
				if freshVIN {
					vin = lastVIN
				}
				if freshBAT {
					vbat = lastVBAT
				}
				isys := int32(0)
				if haveIIn && haveIBat {
					isys = lastIIn - lastIBat
				}
				line := teleJSON(lastTDeci, vbat, vin, isys)
				ringWriteLine(uart0Tx, line)
			}

		// ---- Power values / status / events ----
		case m := <-valCh:
			switch v := m.Payload.(type) {
			case types.BatteryValue:
				lastVBAT = v.PackMilliV
				tsVBAT = time.Now().UnixNano()
			case types.ChargerValue:
				lastVIN = v.VIN_mV
				tsVIN = time.Now().UnixNano()
			}
			printCapValue(m, &lastIIn, &haveIIn, &lastIBat, &haveIBat)
			// mirror the one-line value summary to uart1 (compact, best-effort)
			ringWriteAll(uart1Tx, []byte("[value] power\n"))

		case m := <-stCh:
			printCapStatus(m)

		case m := <-evCh:
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

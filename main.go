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

// ---- thresholds & timing (VIN/VBAT/TEMP) ----

const HAL_TIMEOUT = 5 // second

// Thermal (deci-°C)
const (
	TEMP_LIMIT = 780 // 78.0 °C => force rails OFF
	TEMP_HYST  = 60  // allow ON again at 72.0 °C
)

// Power thresholds (mV)
const (
	// VIN (12 V adapter)
	PG_ON_VIN = 12000 // debounced ON threshold
	SAG_VIN   = 10600 // brownout immediate cut

	// VBAT (12 V SLA)
	PG_ON_VBAT  = 12400 // debounced ON threshold
	PG_OFF_HYST = 800   // OFF ≈ 11.6 V via hysteresis (12400-800)
	SAG_VBAT    = 11400 // brownout immediate cut
)

// Debounce and data freshness
const (
	DEBOUNCE_OK = 400 * time.Millisecond
	STALE_MAX   = 3 * time.Second
)

// ---- topics ----

// HAL
var halReadiness = bus.T("hal", "state")

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

var powerSeq = []RailStep{
	{Name: "mpcie-usb", GapBefore: 200 * time.Millisecond},
	{Name: "m2", GapBefore: 200 * time.Millisecond},
	{Name: "mpcie", GapBefore: 200 * time.Millisecond},
	{Name: "cm5", GapBefore: 200 * time.Millisecond},
	{Name: "fan", GapBefore: 200 * time.Millisecond},
	// Larger gap specifically before boost:
	{Name: "boost-load", GapBefore: 500 * time.Millisecond},
}

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

func main() {
	time.Sleep(3 * time.Second)
	ctx := context.Background()

	log.Println("[main] bootstrapping bus …")
	b := bus.NewBus(4, "+", "#")
	halConn := b.NewConnection("hal")
	uiConn := b.NewConnection("ui")

	log.Println("[main] starting hal.Run …")

	// Start HAL
	go hal.Run(ctx, halConn)

	// Wait for retained hal/state=ready (or time out)
	if !waitHALReady(ctx, halConn, HAL_TIMEOUT*time.Second) {
		for {
			log.Println("[main] HAL not ready within timeout")
			time.Sleep(2 * time.Second)
		}
	}

	// Subscriptions (env + power)
	log.Println("[main] subscribing env + power …")
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

	// State (PG + env + power + UART rings)
	var (
		// UART TX rings
		uart0Tx *shmring.Ring // telemetry (JSON to UART0)

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

		// rails & PG tracking
		pgSince   time.Time
		pgStable  bool
		railsUp   bool
		vbatGood  bool // hysteresis latch for VBAT path
		ledSteady bool // steady when railsUp

		// thermal latch
		otActive bool
	)

	// Single ticker does: LED control, mem stats, PG/TEMP check
	rampTicker := time.NewTicker(2 * time.Second)
	defer rampTicker.Stop()
	const pwmTop = 4095
	levelUp := true // for breathe mode only (railsDown)

	log.Println("[main] entering loop (LED/mem/PG/TEMP tick; env/power prints; UART0 streaming JSON; UART1 logs) …")

	for {
		select {
		// ---- UART session opened/closed ----
		case m := <-subSessOpenTele.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				uart0Tx = shmring.Get(shmring.Handle(ev.TXHandle))
				log.Println("[uart0] telemetry session opened")
			}
		case m := <-subSessOpenLog.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				log.SetUART1(shmring.Get(shmring.Handle(ev.TXHandle)))
				log.Println("[uart1] log session opened")
			}
		case <-subSessClosedTele.Channel():
			uart0Tx = nil
			log.Println("[uart0] telemetry session closed")
			// Auto-reopen
			uiConn.Publish(uiConn.NewMessage(tSessOpen(uartTele), nil, false))
		case <-subSessClosedLog.Channel():
			log.SetUART1(nil)
			log.Println("[uart1] log session closed")
			// Auto-reopen
			uiConn.Publish(uiConn.NewMessage(tSessOpen(uartLog), nil, false))

		// ---- Env prints ----
		case m := <-tempSub.Channel():
			if v, ok := m.Payload.(types.TemperatureValue); ok {
				lastTDeci = int(v.DeciC)
				tsTemp = time.Now().UnixNano()
				log.Deci("[value] env/temperature/core °C=", lastTDeci)

				// JSON: {"env/temperature/core": <deciC>}
				if uart0Tx != nil {
					var w jsonw
					w.r = uart0Tx
					w.begin()
					w.kvInt("env/temperature/core", int(v.DeciC))
					w.end()
				}
			}

		case m := <-humidSub.Channel():
			if v, ok := m.Payload.(types.HumidityValue); ok {
				log.Hundredths("[value] env/humidity/core %RH=", int(v.RHx100))

				// JSON: {"env/humidity/core": <RHx100>}
				if uart0Tx != nil {
					var w jsonw
					w.r = uart0Tx
					w.begin()
					w.kvInt("env/humidity/core", int(v.RHx100))
					w.end()
				}
			}

		// ---- LED + mem + PG/TEMP (one ticker) ----
		case <-rampTicker.C:
			// LED behaviour tied to railsUp:
			// - railsUp => steady ON (send Set once when it flips up)
			// - railsDown => breathe (alternate ramp up/down each tick)
			if railsUp {
				if !ledSteady {
					uiConn.Publish(uiConn.NewMessage(tPWMCtrlSet, types.PWMSet{Level: pwmTop}, false))
					ledSteady = true
				}
			} else {
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

			// JSON memory snapshot: { "sys/mem/mallocs":..,}
			if uart0Tx != nil {
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				var w jsonw
				w.r = uart0Tx
				w.begin()
				w.kvInt("sys/mem/alloc", int(ms.Alloc))
				w.end()
			}

			// ---- Freshness
			now := time.Now()
			freshVIN := tsVIN != 0 && now.Sub(time.Unix(0, tsVIN)) <= STALE_MAX
			freshBAT := tsVBAT != 0 && now.Sub(time.Unix(0, tsVBAT)) <= STALE_MAX
			freshTMP := tsTemp != 0 && now.Sub(time.Unix(0, tsTemp)) <= STALE_MAX

			// ---- Temperature: over-limit latch
			if freshTMP {
				if lastTDeci >= TEMP_LIMIT {
					if !otActive {
						otActive = true
						log.Println("[thermal] over-temp → rails DOWN")
						if railsUp {
							seqDown(uiConn)
							railsUp = false
						}
					}
				} else if lastTDeci <= (TEMP_LIMIT - TEMP_HYST) {
					if otActive {
						log.Println("[thermal] temp recovered below hysteresis")
					}
					otActive = false
				}
			}

			// ---- Temperature: STALE ⇒ immediate rails down
			if !freshTMP && railsUp {
				log.Println("[thermal] temperature stale → rails DOWN")
				seqDown(uiConn)
				railsUp = false
				pgStable = false
				pgSince = time.Time{}
			}

			// ---- VBAT hysteresis latch for PG
			if freshBAT {
				if !vbatGood && lastVBAT >= PG_ON_VBAT {
					vbatGood = true
				} else if vbatGood && lastVBAT < (PG_ON_VBAT-PG_OFF_HYST) {
					vbatGood = false
				}
			} else {
				vbatGood = false // stale VBAT cannot count as good
			}

			// ---- Brownout immediate cut (only if rails are up)
			// A source is OK only if it is fresh AND above SAG.
			if railsUp {
				vinOK := freshVIN && lastVIN >= SAG_VIN
				vbatOK := freshBAT && lastVBAT >= SAG_VBAT
				if !(vinOK || vbatOK) {
					log.Println("[power] brownout or stale on all sources → rails DOWN")
					seqDown(uiConn)
					railsUp = false
					pgStable = false
					pgSince = time.Time{}
				}
			}

			// ---- Power-good decision (debounced turn-on)
			// 1) Supply PG: VIN fresh ≥ PG_ON_VIN OR VBAT hysteresis latch is set.
			pgPG := (freshVIN && lastVIN >= PG_ON_VIN) || vbatGood

			// 2) Temperature gate to *turn on*: must be fresh AND below (LIMIT - HYST).
			tempOK := freshTMP && lastTDeci <= (TEMP_LIMIT-TEMP_HYST)

			if pgPG && tempOK {
				if pgSince.IsZero() {
					pgSince = now
				} else if !pgStable && now.Sub(pgSince) >= DEBOUNCE_OK {
					pgStable = true
					if !railsUp {
						log.Println("[power] PG debounced + Temp OK → rails UP")
						seqUp(uiConn)
						railsUp = true
					}
				}
			} else {
				pgStable = false
				pgSince = time.Time{}
			}

		// ---- Power values / status / events ----
		case m := <-valCh:
			switch v := m.Payload.(type) {
			case types.BatteryValue:
				lastVBAT = v.PackMilliV
				tsVBAT = time.Now().UnixNano()
				lastIBat = v.IBatMilliA
				haveIBat = true

				// JSON: {"power/battery/internal/VBAT":..,"power/battery/internal/IBAT":..}
				if uart0Tx != nil {
					var w jsonw
					w.r = uart0Tx
					w.begin()
					w.kvInt("power/battery/internal/vbat", int(v.PackMilliV))
					w.kvInt("power/battery/internal/ibat", int(v.IBatMilliA))
					w.end()
				}

			case types.ChargerValue:
				lastVIN = v.VIN_mV
				tsVIN = time.Now().UnixNano()
				lastIIn = v.IIn_mA
				haveIIn = true

				// JSON: {"power/charger/internal/VIN":..,"power/charger/internal/VSYS":..,"power/charger/internal/IIN":..}
				if uart0Tx != nil {
					var w jsonw
					w.r = uart0Tx
					w.begin()
					w.kvInt("power/charger/internal/vin", int(v.VIN_mV))
					w.kvInt("power/charger/internal/vsys", int(v.VSYS_mV))
					w.kvInt("power/charger/internal/iin", int(v.IIn_mA))
					w.end()
				}
			}
			printCapValue(m, &lastIIn, &haveIIn, &lastIBat, &haveIBat)

		case m := <-stCh:
			printCapStatus(m)

		case m := <-evCh:
			printCapEvent(m)

			// JSON: {"<dom>/<kind>/<name>/event":"<tag>"}
			if uart0Tx != nil {
				dom, _ := m.Topic.At(2).(string)
				kind, _ := m.Topic.At(3).(string)
				name, _ := m.Topic.At(4).(string)
				tag, _ := m.Topic.At(6).(string)

				var w jsonw
				w.r = uart0Tx
				w.begin()
				w.kvStr(dom+"/"+kind+"/"+name+"/event", tag)
				w.end()
			}

		case <-ctx.Done():
			return
		}
	}
}

// ---- HAL readiness helper (uses retained state) ----
func waitHALReady(ctx context.Context, c *bus.Connection, d time.Duration) bool {
	sub := c.Subscribe(halReadiness)
	defer c.Unsubscribe(sub)

	ctx2, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	for {
		select {
		case m := <-sub.Channel():
			if st, ok := m.Payload.(types.HALState); ok && st.Level == "ready" {
				return true
			}
			// ignore other levels (e.g. "stopped") and keep waiting
		case <-ctx2.Done():
			return false
		}
	}
}

// ----------- power rail sequencing -----------

type RailStep struct {
	Name      string
	GapBefore time.Duration // delay inserted before operating this rail
}

// ---- sequencing helpers ----

// seqUp powers rails in order, inserting each step's configured pre-gap.
// The first step has no initial delay (keeps behaviour intuitive).
func seqUp(uiConn *bus.Connection) {
	first := true
	for _, s := range powerSeq {
		if !first && s.GapBefore > 0 {
			time.Sleep(s.GapBefore)
		}
		log.Println("[event] ", "powering rail UP: ", s.Name)
		uiConn.Publish(uiConn.NewMessage(tSwitch(s.Name), types.SwitchSet{On: true}, false))
		first = false
	}
}

// seqDown powers rails off in reverse order with the same pre-gap policy.
func seqDown(uiConn *bus.Connection) {
	first := true
	for i := len(powerSeq) - 1; i >= 0; i-- {
		s := powerSeq[i]
		if !first && s.GapBefore > 0 {
			time.Sleep(s.GapBefore)
		}
		log.Println("[event] ", "powering rail down: ", s.Name)
		uiConn.Publish(uiConn.NewMessage(tSwitch(s.Name), types.SwitchSet{On: false}, false))
		first = false
	}
}

// ----------- printing helpers (all via Logger) -----------

func printCapValue(m *bus.Message, lastIIn *int32, haveIIn *bool, lastIBat *int32, haveIBat *bool) {
	// hal/cap/<domain>/<kind>/<name>/value
	dom, _ := m.Topic.At(2).(string)
	kind, _ := m.Topic.At(3).(string)
	name, _ := m.Topic.At(4).(string)

	switch v := m.Payload.(type) {
	case types.BatteryValue:
		log.Print("[value] ", dom, "/", kind, "/", name,
			" | VBAT=", int(v.PackMilliV), "mV per=", int(v.PerCellMilliV), "mV | IBAT=", int(v.IBatMilliA), "mA")
		*lastIBat = v.IBatMilliA
		*haveIBat = true
		if *haveIIn && *haveIBat {
			isys := *lastIIn - *lastIBat
			log.Print(" | ISYS≈", int(isys), "mA")
		}
		log.Println()

	case types.ChargerValue:
		log.Print("[value] ", dom, "/", kind, "/", name,
			" | VIN=", int(v.VIN_mV), "mV | VSYS=", int(v.VSYS_mV), "mV | IIN=", int(v.IIn_mA), "mA")
		*lastIIn = v.IIn_mA
		*haveIIn = true
		if *haveIIn && *haveIBat {
			isys := *lastIIn - *lastIBat
			log.Print(" | ISYS≈", int(isys), "mA")
		}
		log.Println()
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

	if sVal, ok := m.Payload.(types.CapabilityStatus); ok {
		log.Println(
			"[link] ", dom, "/", kind, "/", name,
			" | link=", string(sVal.Link),
			" ts=", strconvx.Itoa64(sVal.TS), // note: cast to int for Logger's Itoa
		)
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

	log.Println("[event] ", dom, "/", kind, "/", name, " | ", tag)
}

// -----------------------------------------------------------------------------
// Minimal streaming JSON writer for shmring (no buffers/allocs)
// -----------------------------------------------------------------------------

type jsonw struct {
	r     *shmring.Ring
	first bool
}

func (w *jsonw) begin() {
	w.first = true
	_ = w.r.TryWriteFrom([]byte("{"))
}
func (w *jsonw) end() {
	_ = w.r.TryWriteFrom([]byte("}\r\n"))
}
func (w *jsonw) comma() {
	if !w.first {
		_ = w.r.TryWriteFrom([]byte(","))
	} else {
		w.first = false
	}
}
func (w *jsonw) key(k string) {
	_ = w.r.TryWriteFrom([]byte(`"`))
	_ = w.r.TryWriteFrom([]byte(k))
	_ = w.r.TryWriteFrom([]byte(`":`))
}
func (w *jsonw) kvInt(k string, v int) {
	w.comma()
	w.key(k)
	_ = w.r.TryWriteFrom([]byte(strconvx.Itoa(v)))
}
func (w *jsonw) kvStr(k, s string) {
	w.comma()
	w.key(k)
	_ = w.r.TryWriteFrom([]byte(`"`))
	// very small escape: replace \ and "
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' {
			_ = w.r.TryWriteFrom([]byte{'\\', c})
		} else {
			_ = w.r.TryWriteFrom([]byte{c})
		}
	}
	_ = w.r.TryWriteFrom([]byte(`"`))
}

// -----------------------------------------------------------------------------
// Logger: mirrors every message to USB console and (optionally) uart1.
// No append; emits parts directly. Supports strings, []byte, ints and bools.
// -----------------------------------------------------------------------------

// ---- Logger with hh.hh prefix per line -------------------------------------

type Logger struct {
	target    *shmring.Ring
	t0        time.Time
	lineStart bool
}

var nl = [...]byte{'\r', '\n'}

// Anchor the time origin; also mark that the next write starts a new line.
func (l *Logger) SetStart(t time.Time) { l.t0, l.lineStart = t, true }

func (l *Logger) SetUART1(r *shmring.Ring) { l.target = r }

func (l *Logger) writeString(s string) {
	l.writePrefixIfLineStart()
	if s != "" {
		print(s)
		if l.target != nil {
			_ = l.target.TryWriteFrom([]byte(s))
		}
	}
}

func (l *Logger) writeBytes(b []byte) {
	if len(b) == 0 {
		return
	}
	l.writePrefixIfLineStart()
	print(string(b))
	if l.target != nil {
		_ = l.target.TryWriteFrom(b)
	}
}

// Called at the first output of each line; emits "sss.mmm ".
func (l *Logger) writePrefixIfLineStart() {
	if !l.lineStart {
		return
	}
	l.lineStart = false

	if l.t0.IsZero() {
		l.t0 = time.Now()
	}
	el := time.Since(l.t0)
	secs := int(el / time.Second)
	ms := int((el % time.Second) / time.Millisecond) // 0..999

	// Console (no allocations)
	print(strconvx.Itoa(secs))
	print(".")
	if ms < 100 {
		print("0")
	}
	if ms < 10 {
		print("0")
	}
	print(strconvx.Itoa(ms))
	print(" ")

	// UART1: build once, single write
	if l.target != nil {
		var buf [20]byte
		n := 0
		n += writeDec(buf[:], n, secs)
		buf[n] = '.'
		n++
		n += writeDecPad3(buf[:], n, ms) // zero-padded to 3 digits
		buf[n] = ' '
		n++
		_ = l.target.TryWriteFrom(buf[:n])
	}
}

// Writes v (0..999) as exactly three digits into dst at off. Returns 3.
func writeDecPad3(dst []byte, off int, v int) int {
	if v < 0 {
		v = 0
	} else if v > 999 {
		v = 999
	}
	dst[off+0] = byte('0' + (v/100)%10)
	dst[off+1] = byte('0' + (v/10)%10)
	dst[off+2] = byte('0' + v%10)
	return 3
}

// Decimal writer: appends v into dst at off, returns bytes written.
// Avoids fmt/strconv allocations; v >= 0.
func writeDec(dst []byte, off int, v int) int {
	if v == 0 {
		dst[off] = '0'
		return 1
	}
	var tmp [10]byte
	j := 0
	for v > 0 {
		tmp[j] = byte('0' + v%10)
		v /= 10
		j++
	}
	i := off
	for k := j - 1; k >= 0; k-- {
		dst[i] = tmp[k]
		i++
	}
	return i - off
}

func (l *Logger) writePart(v any) {
	switch x := v.(type) {
	case string:
		l.writeString(x)
	case []byte:
		l.writeBytes(x)
	case int:
		l.writeString(strconvx.Itoa(x))
	case int32:
		l.writeString(strconvx.Itoa(int(x)))
	case int64:
		l.writeString(strconvx.Itoa64(x))
	case uint:
		l.writeString(strconvx.Itoa(int(x)))
	case uint32:
		l.writeString(strconvx.Itoa(int(x)))
	case uint64:
		l.writeString(strconvx.Itoa64(int64(x)))
	case bool:
		if x {
			l.writeString("true")
		} else {
			l.writeString("false")
		}
	default:
		l.writeString("?")
	}
}

func (l *Logger) Print(parts ...any) {
	for i := range parts {
		l.writePart(parts[i])
	}
}

func (l *Logger) newline() {
	print("\r\n")
	if l.target != nil {
		_ = l.target.TryWriteFrom(nl[:])
	}
	l.lineStart = true
}

func (l *Logger) Println(parts ...any) { l.Print(parts...); l.newline() }

// Fixed-point helpers
func (l *Logger) Deci(label string, deci int) {
	l.writePrefixIfLineStart()
	if deci < 0 {
		l.writeString(label)
		l.writeString("-")
		deci = -deci
	} else {
		l.writeString(label)
	}
	whole := deci / 10
	frac := deci % 10
	l.Println(strconvx.Itoa(whole), ".", strconvx.Itoa(frac))
}

func (l *Logger) Hundredths(label string, hx100 int) {
	l.writePrefixIfLineStart()
	if hx100 < 0 {
		hx100 = 0
	}
	whole := hx100 / 100
	frac := hx100 % 100
	if frac < 10 {
		l.Println(label, strconvx.Itoa(whole), ".0", strconvx.Itoa(frac))
	} else {
		l.Println(label, strconvx.Itoa(whole), ".", strconvx.Itoa(frac))
	}
}

// Global instance; ensure first line is prefixed.
var log = Logger{lineStart: true}

// ---- TinyGo runtime memory snapshot ----
func printMem() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	log.Println(
		"[mem] ",
		"alloc:", int(ms.Alloc), " ",
		"heapSys:", int(ms.HeapSys), " ",
		"mallocs:", int(ms.Mallocs), " ",
		"frees:", int(ms.Frees),
	)
}

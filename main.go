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

// -----------------------------------------------------------------------------
// Thresholds & timing
// -----------------------------------------------------------------------------

const halTimeout = 5 * time.Second

// Thermal (deci-°C)
const (
	TEMP_LIMIT = 780 // 78.0 °C => force rails OFF
	TEMP_HYST  = 60  // allow ON again at 72.0 °C
)

// Power thresholds (mV)
const (
	PG_ON_VIN = 12000
	SAG_VIN   = 10600

	PG_ON_VBAT  = 12400
	PG_OFF_HYST = 800
	SAG_VBAT    = 11400
)

// Debounce and data freshness
const (
	DEBOUNCE_OK = 300 * time.Millisecond
	STALE_MAX   = 2 * time.Second
)

// Supervisory cadence
const (
	TICK = 100 * time.Millisecond // balances debounce precision and MCU overhead
)

// LED
const pwmTop = 4095

// -----------------------------------------------------------------------------
// Topics
// -----------------------------------------------------------------------------

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

// -----------------------------------------------------------------------------
// Rail order (pre-gap semantics)
// -----------------------------------------------------------------------------

type RailStep struct {
	Name      string
	GapBefore time.Duration // enforced before operating this rail
}

var powerSeq = []RailStep{
	{Name: "mpcie-usb", GapBefore: 200 * time.Millisecond},
	{Name: "m2", GapBefore: 200 * time.Millisecond},
	{Name: "mpcie", GapBefore: 200 * time.Millisecond},
	{Name: "cm5", GapBefore: 200 * time.Millisecond},
	{Name: "fan", GapBefore: 200 * time.Millisecond},
	{Name: "boost-load", GapBefore: 500 * time.Millisecond},
}

// -----------------------------------------------------------------------------
// Reactor state machine (single goroutine)
// -----------------------------------------------------------------------------

type railsState int

const (
	stateOff railsState = iota
	stateUpSeq
	stateOn
	stateDownSeq
)

type Reactor struct {
	ui *bus.Connection

	// UART
	uart0Tx *shmring.Ring // telemetry (JSON to UART0)
	// Logger UART1 already handled by global logger (see SetUART1)

	// inputs (latest)
	vin_mV, vbat_mV int32
	iin_mA, ibat_mA int32
	lastTDeci       int
	tsVIN, tsVBAT   time.Time
	tsTemp          time.Time

	// derived latches
	vbatGood bool // VBAT hysteresis
	otActive bool // over-temp latch (forces down until recovered)

	// debounce
	pgSince  time.Time
	pgStable bool

	// rails / sequencing
	state         railsState
	seqIdx        int       // index into powerSeq for next action
	seqOnCount    int       // number of rails currently ON
	nextActionDue time.Time // when next rail operation may run

	// LED
	ledSteady bool
	levelUp   bool
	ledTick   int // throttles breathe commands

	// misc
	now time.Time

	// telemetry drops counters
	droppedUART0 int
}

func NewReactor(ui *bus.Connection) *Reactor {
	return &Reactor{
		ui:      ui,
		levelUp: true,
		state:   stateOff,
		now:     time.Now(),
	}
}

// ---- freshness and decisions ----

func (r *Reactor) freshVIN() bool { return !r.tsVIN.IsZero() && r.now.Sub(r.tsVIN) <= STALE_MAX }
func (r *Reactor) freshBAT() bool { return !r.tsVBAT.IsZero() && r.now.Sub(r.tsVBAT) <= STALE_MAX }
func (r *Reactor) freshTMP() bool { return !r.tsTemp.IsZero() && r.now.Sub(r.tsTemp) <= STALE_MAX }

func (r *Reactor) supplyPG() bool {
	// Supply PG for turning on: VIN fresh ≥ PG_ON_VIN OR VBAT hysteresis true.
	return (r.freshVIN() && int(r.vin_mV) >= PG_ON_VIN) || r.vbatGood
}

func (r *Reactor) tempOKForTurnOn() bool {
	// Must be fresh and ≤ LIMIT - HYST
	return r.freshTMP() && r.lastTDeci <= (TEMP_LIMIT-TEMP_HYST)
}

func (r *Reactor) mustCutNow() bool {
	// Immediate cut if: temperature stale OR both sources bad (stale or < SAG) OR over-temp latch.
	if !r.freshTMP() {
		return true
	}
	vinOK := r.freshVIN() && int(r.vin_mV) >= SAG_VIN
	vbatOK := r.freshBAT() && int(r.vbat_mV) >= SAG_VBAT
	return !(vinOK || vbatOK) || r.otActive
}

func (r *Reactor) updateLatchesFromValues() {
	// Over-temp latch
	if r.freshTMP() {
		if r.lastTDeci >= TEMP_LIMIT {
			if !r.otActive {
				log.Println("[thermal] over-temp → latch active")
			}
			r.otActive = true
		} else if r.lastTDeci <= (TEMP_LIMIT - TEMP_HYST) {
			if r.otActive {
				log.Println("[thermal] temp recovered below hysteresis")
			}
			r.otActive = false
		}
	}
	// VBAT hysteresis
	if r.freshBAT() {
		if !r.vbatGood && int(r.vbat_mV) >= PG_ON_VBAT {
			r.vbatGood = true
		} else if r.vbatGood && int(r.vbat_mV) < (PG_ON_VBAT-PG_OFF_HYST) {
			r.vbatGood = false
		}
	} else {
		r.vbatGood = false
	}
}

// ---- sequencing (non-blocking) ----

func (r *Reactor) startUpSeq() {
	log.Println("[power] PG debounced + Temp OK → rails UP")
	r.state = stateUpSeq
	r.seqIdx = 0            // next to apply
	r.nextActionDue = r.now // first step fires immediately
	if r.seqOnCount < 0 {   // safety
		r.seqOnCount = 0
	}
}

func (r *Reactor) startDownSeq() {
	log.Println("[power] brownout/stale/over-temp → rails DOWN")
	r.state = stateDownSeq
	if r.seqOnCount < 0 {
		r.seqOnCount = 0
	}
	if r.seqOnCount > len(powerSeq) {
		r.seqOnCount = len(powerSeq)
	}
	r.seqIdx = r.seqOnCount - 1 // start from last ON rail
	r.nextActionDue = r.now     // first off fires immediately
}

func (r *Reactor) advanceSequenceIfDue() {
	if r.state != stateUpSeq && r.state != stateDownSeq {
		return
	}
	if r.now.Before(r.nextActionDue) {
		return
	}

	switch r.state {
	case stateUpSeq:
		if r.seqIdx >= len(powerSeq) {
			// finished: all rails are on
			r.state = stateOn
			r.seqOnCount = len(powerSeq)
			return
		}
		step := powerSeq[r.seqIdx]
		log.Println("[event] powering rail UP: ", step.Name)
		r.publishSwitch(step.Name, true)
		r.seqOnCount++
		r.seqIdx++
		if r.seqIdx < len(powerSeq) {
			r.nextActionDue = r.now.Add(powerSeq[r.seqIdx].GapBefore)
		}
	case stateDownSeq:
		if r.seqIdx < 0 {
			// finished: all rails are off
			r.state = stateOff
			r.seqOnCount = 0
			return
		}
		step := powerSeq[r.seqIdx]
		log.Println("[event] powering rail down: ", step.Name)
		r.publishSwitch(step.Name, false)
		r.seqOnCount--
		r.seqIdx--
		if r.seqIdx >= 0 {
			r.nextActionDue = r.now.Add(powerSeq[r.seqIdx].GapBefore)
		}
	}
}

func (r *Reactor) publishSwitch(name string, on bool) {
	r.ui.Publish(r.ui.NewMessage(tSwitch(name), types.SwitchSet{On: on}, false))
}

// ---- state transitions (with symmetric reversal) ----

func (r *Reactor) stepFSM() {
	r.updateLatchesFromValues()

	switch r.state {
	case stateOff, stateDownSeq:
		// Evaluate PG/thermal with debounce
		if !r.otActive && r.supplyPG() && r.tempOKForTurnOn() {
			if r.pgSince.IsZero() {
				r.pgSince = r.now
				r.pgStable = false
			} else if !r.pgStable && r.now.Sub(r.pgSince) >= DEBOUNCE_OK {
				r.pgStable = true
			}
		} else {
			r.pgSince = time.Time{}
			r.pgStable = false
		}

		// If actively powering down and inputs become stably good, reverse.
		if r.state == stateDownSeq && r.pgStable {
			log.Println("[power] inputs stably good → reverse to UP sequence")
			r.startUpSeq()
			return
		}
		if r.state == stateOff && r.pgStable {
			r.startUpSeq()
			return
		}

	case stateUpSeq, stateOn:
		if r.mustCutNow() {
			r.startDownSeq()
			return
		}
	}
}

// ---- LED policy tied to rails state ----

func (r *Reactor) stepLED() {
	switch r.state {
	case stateOn:
		r.ledTick = 0
		if !r.ledSteady {
			r.ui.Publish(r.ui.NewMessage(tPWMCtrlSet, types.PWMSet{Level: pwmTop}, false))
			r.ledSteady = true
		}
	default:
		r.ledSteady = false
		r.ledTick++
		if r.ledTick%10 == 0 { // 4 * 250 ms = 1 s
			var target uint16
			if r.levelUp {
				target = pwmTop
			} else {
				target = 0
			}
			r.levelUp = !r.levelUp
			r.ui.Publish(r.ui.NewMessage(tPWMCtrlRamp, types.PWMRamp{To: target, DurationMs: 1000, Steps: 32, Mode: 0}, false))
		}
	}
}

// ---- public input updaters (emit telemetry) ----

func (r *Reactor) OnCharger(v types.ChargerValue) {
	r.vin_mV = v.VIN_mV
	r.iin_mA = v.IIn_mA
	r.tsVIN = r.now

	// JSON: {"power/charger/internal/vin":..,"vsys":..,"iin":..}
	if r.uart0Tx != nil {
		var w jsonw
		w.r = r.uart0Tx
		w.begin()
		w.kvInt("power/charger/internal/vin", int(v.VIN_mV))
		w.kvInt("power/charger/internal/vsys", int(v.VSYS_mV))
		w.kvInt("power/charger/internal/iin", int(v.IIn_mA))
		w.end()
	}
}

func (r *Reactor) OnBattery(v types.BatteryValue) {
	r.vbat_mV = v.PackMilliV
	r.ibat_mA = v.IBatMilliA
	r.tsVBAT = r.now

	// JSON: {"power/battery/internal/vbat":..,"ibat":..}
	if r.uart0Tx != nil {
		var w jsonw
		w.r = r.uart0Tx
		w.begin()
		w.kvInt("power/battery/internal/vbat", int(v.PackMilliV))
		w.kvInt("power/battery/internal/ibat", int(v.IBatMilliA))
		w.end()
	}
}

func (r *Reactor) OnTempDeciC(label string, deci int, jsonKey string) {
	r.lastTDeci = deci
	r.tsTemp = r.now
	log.Deci(label, deci)
	if r.uart0Tx != nil {
		var w jsonw
		w.r = r.uart0Tx
		w.begin()
		w.kvInt(jsonKey, deci)
		w.end()
	}
}

// ---- memory snapshot telemetry (every ~2 s in main loop) ----

func (r *Reactor) emitMemSnapshot() {
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	// log line
	log.Println(
		"[mem] ",
		"alloc:", int(ms.Alloc), " ",
		"heapSys:", int(ms.HeapSys), " ",
		"mallocs:", int(ms.Mallocs), " ",
		"frees:", int(ms.Frees),
	)
	// JSON (minimal to keep overhead low)
	if r.uart0Tx != nil {
		var w jsonw
		w.r = r.uart0Tx
		w.begin()
		w.kvInt("sys/mem/alloc", int(ms.Alloc))
		w.end()
	}
}

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	// Allow early USB/console settle if needed
	time.Sleep(3 * time.Second)
	log.SetStart(time.Now())

	ctx := context.Background()

	log.Println("[main] bootstrapping bus …")
	b := bus.NewBus(4, "+", "#")
	halConn := b.NewConnection("hal")
	uiConn := b.NewConnection("ui")

	log.Println("[main] starting hal.Run …")
	go hal.Run(ctx, halConn)

	// Wait for retained hal/state=ready (or time out)
	if !waitHALReady(ctx, halConn, halTimeout) {
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

	// Retry back-off guards
	var retryTeleAt, retryLogAt time.Time

	// Reactor
	r := NewReactor(uiConn)

	// Supervisory ticker
	ticker := time.NewTicker(TICK)
	defer ticker.Stop()
	memTick := 0

	log.Println("[main] entering reactor loop …")
	for {
		select {
		// ---- UART session opened/closed ----
		case m := <-subSessOpenTele.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				r.uart0Tx = shmring.Get(shmring.Handle(ev.TXHandle))
				log.Println("[uart0] telemetry session opened")
			}
		case m := <-subSessOpenLog.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				log.SetUART1(shmring.Get(shmring.Handle(ev.TXHandle)))
				log.Println("[uart1] log session opened")
			}
		case <-subSessClosedTele.Channel():
			r.uart0Tx = nil
			log.Println("[uart0] telemetry session closed")
			// Auto-reopen with back-off
			if time.Now().After(retryTeleAt) {
				uiConn.Publish(uiConn.NewMessage(tSessOpen(uartTele), nil, false))
				retryTeleAt = time.Now().Add(2 * time.Second)
			}
		case <-subSessClosedLog.Channel():
			log.SetUART1(nil)
			log.Println("[uart1] log session closed")
			// Auto-reopen with back-off
			if time.Now().After(retryLogAt) {
				uiConn.Publish(uiConn.NewMessage(tSessOpen(uartLog), nil, false))
				retryLogAt = time.Now().Add(2 * time.Second)
			}

		// ---- Env prints ----
		case m := <-tempSub.Channel():
			if v, ok := m.Payload.(types.TemperatureValue); ok {
				r.now = time.Now()
				r.OnTempDeciC("[value] env/temperature/core °C=", int(v.DeciC), "env/temperature/core")
			}
		case m := <-humidSub.Channel():
			if v, ok := m.Payload.(types.HumidityValue); ok {
				log.Hundredths("[value] env/humidity/core %RH=", int(v.RHx100))
				// JSON
				if r.uart0Tx != nil {
					var w jsonw
					w.r = r.uart0Tx
					w.begin()
					w.kvInt("env/humidity/core", int(v.RHx100))
					w.end()
				}
			}

		// ---- Power values / status / events ----
		case m := <-valSub.Channel():
			r.now = time.Now()
			switch v := m.Payload.(type) {
			case types.BatteryValue:
				r.OnBattery(v)
				printCapValue(m, &r.iin_mA, nil, &r.ibat_mA, nil)
			case types.ChargerValue:
				r.OnCharger(v)
				printCapValue(m, &r.iin_mA, nil, &r.ibat_mA, nil)
			case types.TemperatureValue:
				r.OnTempDeciC("[value] power/temperature/internal °C=", int(v.DeciC), "power/temperature/internal")
			}

		case m := <-stSub.Channel():
			printCapStatus(m)

		case m := <-evSub.Channel():
			printCapEvent(m)
			// JSON: {"<dom>/<kind>/<name>/event":"<tag>"}
			if r.uart0Tx != nil {
				dom, _ := m.Topic.At(2).(string)
				kind, _ := m.Topic.At(3).(string)
				name, _ := m.Topic.At(4).(string)
				tag, _ := m.Topic.At(6).(string)
				if dom != "" && kind != "" && name != "" && tag != "" {
					var w jsonw
					w.r = r.uart0Tx
					w.begin()
					w.kvStr(dom+"/"+kind+"/"+name+"/event", tag)
					w.end()
				}
			}

		// ---- Supervisory tick ----
		case <-ticker.C:
			r.now = time.Now()

			// 1) Run FSM (includes symmetric reversal)
			r.stepFSM()

			// 2) Advance sequencing steps if due
			r.advanceSequenceIfDue()

			// 3) LED behaviour
			r.stepLED()

			// 4) Periodic memory snapshot (~2 s)
			memTick++
			if memTick%30 == 0 { // 8 * 250 ms = 2 s
				r.emitMemSnapshot()
			}
		case <-ctx.Done():
			return
		}
	}
}

// -----------------------------------------------------------------------------
// HAL readiness helper (retained)
// -----------------------------------------------------------------------------

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
		case <-ctx2.Done():
			return false
		}
	}
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
	if w.r != nil {
		_ = w.r.TryWriteFrom([]byte("{"))
	}
}
func (w *jsonw) end() {
	if w.r != nil {
		_ = w.r.TryWriteFrom([]byte("}\n"))
	}
}
func (w *jsonw) comma() {
	if w.r == nil {
		return
	}
	if !w.first {
		_ = w.r.TryWriteFrom([]byte(","))
	} else {
		w.first = false
	}
}
func (w *jsonw) key(k string) {
	if w.r == nil {
		return
	}
	_ = w.r.TryWriteFrom([]byte(`"`))
	_ = w.r.TryWriteFrom([]byte(k))
	_ = w.r.TryWriteFrom([]byte(`":`))
}
func (w *jsonw) kvInt(k string, v int) {
	w.comma()
	w.key(k)
	if w.r != nil {
		_ = w.r.TryWriteFrom([]byte(strconvx.Itoa(v)))
	}
}
func (w *jsonw) kvStr(k, s string) {
	w.comma()
	w.key(k)
	if w.r == nil {
		return
	}
	_ = w.r.TryWriteFrom([]byte(`"`))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '"':
			_ = w.r.TryWriteFrom([]byte{'\\', c})
		case '\b':
			_ = w.r.TryWriteFrom([]byte{'\\', 'b'})
		case '\f':
			_ = w.r.TryWriteFrom([]byte{'\\', 'f'})
		case '\n':
			_ = w.r.TryWriteFrom([]byte{'\\', 'n'})
		case '\r':
			_ = w.r.TryWriteFrom([]byte{'\\', 'r'})
		case '\t':
			_ = w.r.TryWriteFrom([]byte{'\\', 't'})
		default:
			if c < 0x20 {
				// \u00XX escape for other controls
				var buf [6]byte
				buf[0], buf[1], buf[2], buf[3] = '\\', 'u', '0', '0'
				hex := "0123456789abcdef"
				buf[4] = hex[c>>4]
				buf[5] = hex[c&0xF]
				_ = w.r.TryWriteFrom(buf[:])
			} else {
				_ = w.r.TryWriteFrom([]byte{c})
			}
		}
	}
	_ = w.r.TryWriteFrom([]byte(`"`))
}

// -----------------------------------------------------------------------------
// Printing helpers (via Logger)
// -----------------------------------------------------------------------------

func printCapValue(m *bus.Message, lastIIn *int32, _ *bool, lastIBat *int32, _ *bool) {
	// hal/cap/<domain>/<kind>/<name>/value
	dom, _ := m.Topic.At(2).(string)
	kind, _ := m.Topic.At(3).(string)
	name, _ := m.Topic.At(4).(string)

	switch v := m.Payload.(type) {
	case types.BatteryValue:
		log.Print("[value] ", dom, "/", kind, "/", name,
			" | VBAT=", int(v.PackMilliV), "mV per=", int(v.PerCellMilliV), "mV | IBAT=", int(v.IBatMilliA), "mA")
		if lastIBat != nil {
			*lastIBat = v.IBatMilliA
		}
		if lastIIn != nil {
			isys := *lastIIn - v.IBatMilliA
			log.Print(" | ISYS≈", int(isys), "mA")
		}
		log.Println()
	case types.ChargerValue:
		log.Print("[value] ", dom, "/", kind, "/", name,
			" | VIN=", int(v.VIN_mV), "mV | VSYS=", int(v.VSYS_mV), "mV | IIN=", int(v.IIn_mA), "mA")
		if lastIIn != nil {
			*lastIIn = v.IIn_mA
			if lastIBat != nil {
				isys := *lastIIn - *lastIBat
				log.Print(" | ISYS≈", int(isys), "mA")
			}
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
			" ts=", strconvx.Itoa64(sVal.TS),
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
// Logger (mirrors to USB console and optionally uart1). No heap churn.
// -----------------------------------------------------------------------------

type Logger struct {
	target    *shmring.Ring
	t0        time.Time
	lineStart bool
}

var nl = [...]byte{'\n'}

func (l *Logger) SetStart(t time.Time)     { l.t0, l.lineStart = t, true }
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
		n += writeDecPad3(buf[:], n, ms)
		buf[n] = ' '
		n++
		_ = l.target.TryWriteFrom(buf[:n])
	}
}
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
	print("\n")
	if l.target != nil {
		_ = l.target.TryWriteFrom(nl[:])
	}
	l.lineStart = true
}
func (l *Logger) Println(parts ...any) { l.Print(parts...); l.newline() }

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

// Global logger instance
var log = Logger{lineStart: true}

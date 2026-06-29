//go:build !qa_reactor

package reactor

import (
	"context"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/updater"
	"devicecode-go/types"
	"devicecode-go/utilities"
	"devicecode-go/utilities/diag"
	"devicecode-go/x/shmring"
	"devicecode-go/x/strconvx"
)

// -----------------------------------------------------------------------------
// Thresholds & timing
// -----------------------------------------------------------------------------

const pwmTop = 4095

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
	DEBOUNCE_OK       = 300 * time.Millisecond
	STALE_MAX         = 4 * time.Second
	DIE_TEMP_TAKEOVER = 2 * time.Second
)

// Supervisory cadence
const (
	TICK = 100 * time.Millisecond // balances debounce precision and MCU overhead
)

// -----------------------------------------------------------------------------
// AHT20 readiness (for boards where the AHT isn't functioning)
// -----------------------------------------------------------------------------

var aht20Alive = false

// -----------------------------------------------------------------------------
// Topics
// -----------------------------------------------------------------------------

// LED
var (
	tPWMCtrlSet  = bus.T("hal", "cap", "io", string(types.KindPWM), "button_led", "control", "set")
	tPWMCtrlRamp = bus.T("hal", "cap", "io", string(types.KindPWM), "button_led", "control", "ramp")
)

// Die
var tDieTempValue = bus.T("hal", "cap", "env", string(types.KindTemperature), "die", "value")

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

// UART sessions. Precompute the fixed reactor topics so boot and retry paths
// do not construct topic slices or touch the interner.
const uartLog = "uart1"

var (
	tSessOpenLog      = bus.T("hal", "cap", "io", "serial", uartLog, "control", "session_open")
	tSessOpenedLog    = bus.T("hal", "cap", "io", "serial", uartLog, "event", "session_opened")
	tSessClosedLog    = bus.T("hal", "cap", "io", "serial", uartLog, "event", "session_closed")
	tSessOpenFabric   = bus.T("hal", "cap", "io", "serial", fabricUART, "control", "session_open")
	tSessOpenedFabric = bus.T("hal", "cap", "io", "serial", fabricUART, "event", "session_opened")
	tSessClosedFabric = bus.T("hal", "cap", "io", "serial", fabricUART, "event", "session_closed")
)

func subscriptionChannel(sub *bus.Subscription) <-chan bus.Message {
	if sub == nil {
		return nil
	}
	return sub.Channel()
}

// -----------------------------------------------------------------------------
// Rail order (pre-gap semantics)
// -----------------------------------------------------------------------------

type RailStep struct {
	Name      string
	Topic     bus.Topic
	GapBefore time.Duration // enforced before operating this rail
}

var powerSeq = []RailStep{
	{Name: "mpcie-usb", Topic: bus.T("hal", "cap", "power", string(types.KindSwitch), "mpcie-usb", "control", "set"), GapBefore: 200 * time.Millisecond},
	{Name: "m2", Topic: bus.T("hal", "cap", "power", string(types.KindSwitch), "m2", "control", "set"), GapBefore: 200 * time.Millisecond},
	{Name: "mpcie", Topic: bus.T("hal", "cap", "power", string(types.KindSwitch), "mpcie", "control", "set"), GapBefore: 200 * time.Millisecond},
	{Name: "cm5", Topic: bus.T("hal", "cap", "power", string(types.KindSwitch), "cm5", "control", "set"), GapBefore: 200 * time.Millisecond},
	{Name: "fan", Topic: bus.T("hal", "cap", "power", string(types.KindSwitch), "fan", "control", "set"), GapBefore: 200 * time.Millisecond},
	{Name: "boost-load", Topic: bus.T("hal", "cap", "power", string(types.KindSwitch), "boost-load", "control", "set"), GapBefore: 500 * time.Millisecond},
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
	uiConn *bus.Connection

	// UART
	// Fabric uses uart0; human-readable logs are mirrored to uart1.

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

	// telemetry drop counters (bytes)
	droppedUART0Bytes int

	// supervised children. The Reactor owns only lifecycle; child
	// services own their own event loops and models.
	children   childSupervisor
	updaterSvc *updater.Service

	// Fabric link lifecycle. Fabric owns its protocol reactor; this top-level
	// Reactor only opens/closes the HAL UART session and cancels the active
	// Fabric session when the HAL session is replaced or closed.
	fabricCancel      context.CancelFunc
	fabricDone        chan struct{}
	fabricSessionOpen bool
}

func NewReactor(uiConn *bus.Connection) *Reactor {
	return &Reactor{
		uiConn:  uiConn,
		levelUp: true,
		state:   stateOff,
		now:     time.Now(),
		ledTick: 0,
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
				log.Println("thermal hot")
			}
			r.otActive = true
		} else if r.lastTDeci <= (TEMP_LIMIT - TEMP_HYST) {
			if r.otActive {
				log.Println("thermal ok")
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
	log.Println("pwr up")
	r.state = stateUpSeq
	r.seqIdx = 0            // next to apply
	r.nextActionDue = r.now // first step fires immediately
	if r.seqOnCount < 0 {   // safety
		r.seqOnCount = 0
	}
}

func (r *Reactor) startDownSeq() {
	log.Println("pwr down")
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
		log.Println("rail+ ", step.Name)
		r.publishSwitch(step, true)
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
		log.Println("rail- ", step.Name)
		r.publishSwitch(step, false)
		r.seqOnCount--
		r.seqIdx--
		if r.seqIdx >= 0 {
			r.nextActionDue = r.now.Add(powerSeq[r.seqIdx].GapBefore)
		}
	}
}

func (r *Reactor) publishSwitch(step RailStep, on bool) {
	r.uiConn.PublishValue(step.Topic, types.SwitchSet{On: on}, false)
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
			log.Println("pwr reverse-up")
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
			// Steady ON on healthy rails
			r.uiConn.PublishValue(tPWMCtrlSet, types.PWMSet{Level: pwmTop}, false)
			r.ledSteady = true
		}
	default:
		r.ledSteady = false
		r.ledTick++
		if r.ledTick%10 == 0 { // 10 * 100 ms = 1 s
			var target uint16
			if r.levelUp {
				target = pwmTop
			} else {
				target = 0
			}
			r.levelUp = !r.levelUp
			r.uiConn.PublishValue(tPWMCtrlRamp, types.PWMRamp{To: target, DurationMs: 1000, Steps: 32, Mode: 0}, false)
		}
	}
}

// ---- public input updaters (emit telemetry) ----

func (r *Reactor) OnCharger(v types.ChargerValue) {
	r.vin_mV = v.VIN_mV
	r.iin_mA = v.IIn_mA
	r.tsVIN = r.now

	// JSON: {"power/charger/internal/vin":..,"vsys":..,"iin":..}
}

func (r *Reactor) OnBattery(v types.BatteryValue) {
	r.vbat_mV = v.PackMilliV
	r.ibat_mA = v.IBatMilliA
	r.tsVBAT = r.now

	// JSON: {"power/battery/internal/vbat":..,"ibat":..}
}

func (r *Reactor) OnTempDeciC(label string, deci int, jsonKey string) {
	log.Deci(label, deci)
}

func (r *Reactor) Run(ctx context.Context) {
	r.startCoreChildren(ctx)
	defer r.stopCoreChildren()
	defer r.stopFabricLink()

	// Subscriptions (env + power)
	log.Println("reactor sub")
	tempSub := r.uiConn.Subscribe(tTempValue)
	tempDieSub := r.uiConn.Subscribe(tDieTempValue)
	humidSub := r.uiConn.Subscribe(tHumValue)
	valSub := r.uiConn.Subscribe(valTopic)
	stSub := r.uiConn.Subscribe(stTopic)
	evSub := r.uiConn.Subscribe(evTopic)

	// UART sessions. uart0 is the CM5 Fabric/message-bus link; uart1 is
	// reserved for human-readable diagnostics. Legacy JSON telemetry is not
	// emitted on either UART.
	subSessOpenLog := r.uiConn.Subscribe(tSessOpenedLog)
	subSessClosedLog := r.uiConn.Subscribe(tSessClosedLog)
	var subSessOpenFabric *bus.Subscription
	var subSessClosedFabric *bus.Subscription
	if useHardwareFabricUART() {
		subSessOpenFabric = r.uiConn.Subscribe(tSessOpenedFabric)
		subSessClosedFabric = r.uiConn.Subscribe(tSessClosedFabric)
	}

	// Kick open requests (fire-and-forget; events carry handles).
	r.uiConn.PublishValue(tSessOpenLog, nil, false)
	if useHardwareFabricUART() {
		r.uiConn.PublishValue(tSessOpenFabric, nil, false)
	}

	// Retry back-off guards.
	var retryLogAt, retryFabricAt time.Time

	// Supervisory ticker
	ticker := time.NewTicker(TICK)
	defer ticker.Stop()

	log.Println("reactor run")
	for {
		select {
		// ---- UART session opened/closed ----
		case m := <-subSessOpenLog.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				ring := shmring.Get(shmring.Handle(ev.TXHandle))
				log.SetUART1(ring)
				diag.SetUART1(ring)
				log.Println("uart1 open")
			}
		case m := <-subscriptionChannel(subSessOpenFabric):
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				r.startPassiveFabric(ctx, ev)
			}
		case <-subSessClosedLog.Channel():
			log.SetUART1(nil)
			diag.SetUART1(nil)
			log.Println("uart1 closed")
			// Auto-reopen with back-off
			if time.Now().After(retryLogAt) {
				r.uiConn.PublishValue(tSessOpenLog, nil, false)
				retryLogAt = time.Now().Add(2 * time.Second)
			}
		case <-subscriptionChannel(subSessClosedFabric):
			r.stopFabricLink()
			log.Println(fabricLogPrefix + "closed")
			// Auto-reopen with back-off
			if time.Now().After(retryFabricAt) {
				r.uiConn.PublishValue(tSessOpenFabric, nil, false)
				retryFabricAt = time.Now().Add(2 * time.Second)
			}

		// ---- Env prints ----
		case m := <-tempSub.Channel():
			if v, ok := m.Payload.(types.TemperatureValue); ok {
				if !aht20Alive {
					aht20Alive = true
				}
				r.now = time.Now()
				deci := int(v.DeciC)
				r.lastTDeci = deci
				r.tsTemp = r.now
				r.OnTempDeciC("envT=", deci, "env/temperature/core")
			}
		case m := <-humidSub.Channel():
			if v, ok := m.Payload.(types.HumidityValue); ok {
				log.Hundredths("envH=", int(v.RHx100))
				// JSON
			}

		// ---- Die Temp Backup ----
		case m := <-tempDieSub.Channel():
			if v, ok := m.Payload.(types.TemperatureValue); ok {
				r.now = time.Now()
				deci := int(v.DeciC)
				if !aht20Alive || (r.now.Sub(r.tsTemp) > DIE_TEMP_TAKEOVER) {
					aht20Alive = false
					r.lastTDeci = deci
					r.tsTemp = r.now
					r.OnTempDeciC("envT=", deci, "env/temperature/core")
				}
			}

		// ---- Power values / status / events ----
		case m := <-valSub.Channel():
			r.now = time.Now()
			switch v := m.Payload.(type) {
			case types.BatteryValue:
				r.OnBattery(v)
				printCapValue(&m, &r.iin_mA, nil, &r.ibat_mA, nil)
			case types.ChargerValue:
				r.OnCharger(v)
				printCapValue(&m, &r.iin_mA, nil, &r.ibat_mA, nil)
			case types.TemperatureValue:
				r.OnTempDeciC("pwrT=", int(v.DeciC), "power/temperature/internal")
			}

		case m := <-stSub.Channel():
			printCapStatus(&m)

		case m := <-evSub.Channel():
			printCapEvent(&m)
			// JSON: {"<dom>/<kind>/<name>/event":"<tag>"}

		// ---- Child service lifecycle ----
		case ev := <-r.children.Done():
			r.children.HandleExit(ev)

		// ---- Supervisory tick ----
		case <-ticker.C:
			r.now = time.Now()

			// 1) Run FSM (includes symmetric reversal)
			r.stepFSM()

			// 2) Advance sequencing steps if due
			r.advanceSequenceIfDue()

			// 3) LED behaviour
			r.stepLED()

		case <-ctx.Done():
			return
		}
	}
}

// -----------------------------------------------------------------------------
// Centralised UART write helpers (handle partial writes)
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// Printing helpers (via Logger)
// -----------------------------------------------------------------------------

func printCapValue(m *bus.Message, lastIIn *int32, _ *bool, lastIBat *int32, _ *bool) {
	switch v := m.Payload.(type) {
	case types.BatteryValue:
		log.Print("bat v=", int(v.PackMilliV), " cell=", int(v.PerCellMilliV), " i=", int(v.IBatMilliA), " bsr=", int(v.BSR_uOhmPerCell))
		if lastIBat != nil {
			*lastIBat = v.IBatMilliA
		}
		if lastIIn != nil {
			log.Print(" isys=", int(*lastIIn-v.IBatMilliA))
		}
		log.Println()
	case types.ChargerValue:
		log.Print("chg vin=", int(v.VIN_mV), " vsys=", int(v.VSYS_mV), " iin=", int(v.IIn_mA))
		if lastIIn != nil {
			*lastIIn = v.IIn_mA
			if lastIBat != nil {
				log.Print(" isys=", int(*lastIIn-*lastIBat))
			}
		}
		log.Print(" st=")
		logHex16(v.State)
		log.Print(" ss=")
		logHex16(v.Status)
		log.Print(" sys=")
		logHex16(v.Sys)
		log.Println()
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

	log.Println("ev ", kind, "/", name, " ", tag)
}

// Global logger instance
var log = utilities.Logger{LineStart: true}

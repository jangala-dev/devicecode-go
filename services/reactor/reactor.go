//go:build !qa_reactor

package reactor

import (
	"context"
	"runtime"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/fabric"
	"devicecode-go/services/telemetry"
	"devicecode-go/services/updater"
	"devicecode-go/types"
	"devicecode-go/utilities"
	"devicecode-go/x/shmring"
	"devicecode-go/x/strconvx"
)

// FirmwareVersion/FirmwareBuild/FirmwareImageID are the stamps the updater
// publishes via state/self/software. main may override them before the reactor
// starts; defaults are development sentinels.
var (
	FirmwareVersion = "0.0.0-dev"
	FirmwareBuild   = "local"
	FirmwareImageID = "img-dev"
)

func firmwareIdentity() updater.Identity {
	return updater.Identity{
		Version: FirmwareVersion,
		Build:   FirmwareBuild,
		ImageID: FirmwareImageID,
	}
}

const (
	fabricWaitLogInterval = 2 * time.Second
	fabricStopWaitTimeout = 500 * time.Millisecond
)

func waitFabricDone(done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func waitForUpdaterCriticalFacts(ctx context.Context, conn *bus.Connection) bool {
	if conn == nil {
		return false
	}
	swSub := conn.Subscribe(updater.TopicSoftwareFact)
	defer conn.Unsubscribe(swSub)
	upSub := conn.Subscribe(updater.TopicUpdaterFact)
	defer conn.Unsubscribe(upSub)
	healthSub := conn.Subscribe(updater.TopicHealthFact)
	defer conn.Unsubscribe(healthSub)

	softwareReady := false
	updaterReady := false
	healthReady := false

	for !(softwareReady && updaterReady && healthReady) {
		select {
		case <-ctx.Done():
			return false
		case msg, ok := <-swSub.Channel():
			if ok && msg != nil && msg.Payload != nil {
				softwareReady = true
			}
		case msg, ok := <-upSub.Channel():
			if ok && msg != nil && msg.Payload != nil {
				updaterReady = true
			}
		case msg, ok := <-healthSub.Channel():
			if ok && msg != nil && msg.Payload != nil {
				healthReady = true
			}
		}
	}
	return true
}

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
	bus    *bus.Bus
	uiConn *bus.Connection

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
	now       time.Time
	bootBuyRC int32

	// updater service handle used by the post-hello_ack republish hook.
	updater *updater.Service
}

type Options struct {
	BootBuyRC int32
}

func NewReactor(b *bus.Bus, uiConn *bus.Connection) *Reactor {
	return NewReactorWithOptions(b, uiConn, Options{})
}

func NewReactorWithOptions(b *bus.Bus, uiConn *bus.Connection, opts Options) *Reactor {
	return &Reactor{
		bus:       b,
		uiConn:    uiConn,
		levelUp:   true,
		state:     stateOff,
		now:       time.Now(),
		bootBuyRC: opts.BootBuyRC,
		ledTick:   0,
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
	r.uiConn.Publish(r.uiConn.NewMessage(tSwitch(name), types.SwitchSet{On: on}, false))
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
			// Steady ON on healthy rails
			r.uiConn.Publish(r.uiConn.NewMessage(tPWMCtrlSet, types.PWMSet{Level: pwmTop}, false))
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
			r.uiConn.Publish(r.uiConn.NewMessage(tPWMCtrlRamp, types.PWMRamp{To: target, DurationMs: 1000, Steps: 32, Mode: 0}, false))
		}
	}
}

// ---- public input updaters (emit telemetry) ----

func (r *Reactor) OnCharger(v types.ChargerValue) {
	r.vin_mV = v.VIN_mV
	r.iin_mA = v.IIn_mA
	r.tsVIN = r.now
}

func (r *Reactor) OnBattery(v types.BatteryValue) {
	r.vbat_mV = v.PackMilliV
	r.ibat_mA = v.IBatMilliA
	r.tsVBAT = r.now
}

func (r *Reactor) OnTempDeciC(label string, deci int, _ string) {
	log.Deci(label, deci)
}

// ---- memory snapshot (every ~3 s in main loop) ----

func (r *Reactor) emitMemSnapshot() {
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

func (r *Reactor) Run(ctx context.Context) {
	// Updater service: state machine + updater prepare/commit RPC
	// RPC handlers + updater/main staging + retained state/self/{software,
	// updater, health} facts. Started early so the initial fact retains
	// land before fabric establishes — that way the first hello_ack
	// observer sees a populated retain store.
	updaterConn := r.bus.NewConnection("updater")
	identity := firmwareIdentity()
	updaterSvc := updater.New(updater.Options{
		Conn:      updaterConn,
		Verifier:  updater.SignedImageVerifier(),
		Applier:   updater.ProductionApplier(),
		Identity:  identity,
		BootBuyRC: r.bootBuyRC,
	})
	go updaterSvc.Run(ctx)
	r.updater = updaterSvc
	if !waitForUpdaterCriticalFacts(ctx, r.bus.NewConnection("updater-ready")) {
		return
	}

	// Telemetry service: subscribes to HAL value topics and republishes
	// at state/self/* with integer engineering units; runs the charger
	// alert FSM and emits event/self/power/charger/alert on bit-set
	// transitions. Started after the updater so the initial software/
	// updater retains land first.
	telemetryConn := r.bus.NewConnection("telemetry")
	telemetrySvc := telemetry.New(telemetryConn)
	go telemetrySvc.Run(ctx)

	// Subscriptions (env + power)
	log.Println("[main] subscribing env + power …")
	tempSub := r.uiConn.Subscribe(tTempValue)
	tempDieSub := r.uiConn.Subscribe(tDieTempValue)
	humidSub := r.uiConn.Subscribe(tHumValue)
	valSub := r.uiConn.Subscribe(valTopic)
	stSub := r.uiConn.Subscribe(stTopic)
	evSub := r.uiConn.Subscribe(evTopic)

	// UART session for the CM5 Fabric link on proto_1 hardware.
	const uartFabric = "uart1"
	subSessOpenFabric := r.uiConn.Subscribe(tSessOpened(uartFabric))
	subSessClosedFabric := r.uiConn.Subscribe(tSessClosed(uartFabric))
	r.uiConn.Publish(r.uiConn.NewMessage(tSessOpen(uartFabric), nil, false))

	// Retry back-off guards
	var retryFabricAt time.Time

	// Fabric session lifecycle state
	var fabricCancel context.CancelFunc
	var fabricDone chan struct{}
	var fabricSessionOpen bool
	nextFabricWaitLog := time.Now()

	stopFabricSession := func() {
		if fabricCancel == nil {
			return
		}
		done := fabricDone
		fabricCancel()
		fabricCancel = nil
		fabricDone = nil
		if !waitFabricDone(done, fabricStopWaitTimeout) {
			log.Println("[uart1] fabric session stop timed out")
		}
	}

	// Supervisory ticker
	ticker := time.NewTicker(TICK)
	defer ticker.Stop()
	memTick := 0

	log.Println("[main] entering reactor loop …")
	for {
		select {
		// ---- UART session opened/closed ----
		case m := <-subSessOpenFabric.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				// Tear down any previous fabric session before starting a new one.
				stopFabricSession()
				rx := shmring.Get(shmring.Handle(ev.RXHandle))
				tx := shmring.Get(shmring.Handle(ev.TXHandle))
				tr := fabric.NewShmringTransport(rx, tx)
				fabricConn := r.bus.NewConnection("fabric")
				fabricCtx, cancel := context.WithCancel(ctx)
				done := make(chan struct{})
				fabricCancel = cancel
				fabricDone = done
				fabricSessionOpen = true
				log.Println("[uart1] fabric session opening node=mcu peer=bigbox-cm5 link=mcu-uart0")
				go func() {
					defer close(done)
					fabric.Run(fabricCtx, tr, fabricConn, "mcu", "bigbox-cm5", fabric.DefaultLinkConfig())
				}()
				log.Println("[uart1] fabric session opened node=mcu peer=bigbox-cm5 link=mcu-uart0")
			}
		case <-subSessClosedFabric.Channel():
			// Ignore stale close events — the open handler already tears down
			// the previous session before starting a new one.
			if !fabricSessionOpen {
				continue
			}
			stopFabricSession()
			fabricSessionOpen = false
			nextFabricWaitLog = time.Now()
			log.Println("[uart1] fabric session closed")
			if time.Now().After(retryFabricAt) {
				r.uiConn.Publish(r.uiConn.NewMessage(tSessOpen(uartFabric), nil, false))
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
				r.OnTempDeciC("[value] env/temperature/core °C=", deci, "env/temperature/core")
			}
		case m := <-humidSub.Channel():
			if v, ok := m.Payload.(types.HumidityValue); ok {
				log.Hundredths("[value] env/humidity/core %RH=", int(v.RHx100))
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
					r.OnTempDeciC("[value] env/temperature/core °C=", deci, "env/temperature/core")
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

		// ---- Supervisory tick ----
		case <-ticker.C:
			r.now = time.Now()

			if !fabricSessionOpen && !r.now.Before(nextFabricWaitLog) {
				log.Println("[main] waiting for fabric connection start")
				nextFabricWaitLog = r.now.Add(fabricWaitLogInterval)
			}

			// 1) Run FSM (includes symmetric reversal)
			r.stepFSM()

			// 2) Advance sequencing steps if due
			r.advanceSequenceIfDue()

			// 3) LED behaviour
			r.stepLED()

			// 4) Periodic memory snapshot (~3 s)
			memTick++
			if memTick%30 == 0 { // 30 * 100 ms = 3 s
				r.emitMemSnapshot()
			}
		case <-ctx.Done():
			return
		}
	}
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
			" | VBAT=", int(v.PackMilliV), "mV per=", int(v.PerCellMilliV), "mV | IBAT=", int(v.IBatMilliA), "mA | BSR=", int(v.BSR_uOhmPerCell), "uR")
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
		// ---- human-readable (SET bits only) ----
		{
			it := types.NewBitIter(types.SystemStatus(v.Sys), types.SystemStatusTable[:])
			first := true
			log.Print(" | system=[")
			for name, ok := it.Next(); ok; name, ok = it.Next() {
				if !first {
					log.Print(",")
				} else {
					first = false
				}
				log.Print(name)
			}
			log.Print("]")
		}
		{
			it := types.NewBitIter(types.ChargeStatusBits(v.Status), types.ChargeStatusTable[:])
			first := true
			log.Print(" | status=[")
			for name, ok := it.Next(); ok; name, ok = it.Next() {
				if !first {
					log.Print(",")
				} else {
					first = false
				}
				log.Print(name)
			}
			log.Print("]")
		}
		{
			it := types.NewBitIter(types.ChargerStateBits(v.State), types.ChargerStateTable[:])
			first := true
			log.Print(" | state=[")
			for name, ok := it.Next(); ok; name, ok = it.Next() {
				if !first {
					log.Print(",")
				} else {
					first = false
				}
				log.Print(name)
			}
			log.Print("]")
		}
		log.Println()

	default:
		// ignore others
	}
}

// helper: prefix for status lines (keeps logger zero-alloc style)
func (r *Reactor) logPrefixStatus(path, label string) {
	log.Print("[status] ", path, " ", label, ": ")
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

// Global logger instance
var log = utilities.Logger{LineStart: true}

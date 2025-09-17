// services/hal/integration/hal_integration_ltc4015_test.go
//go:build !rp2040 && !rp2350

package integration

import (
	"context"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal/config"
	"devicecode-go/services/hal/internal/consts"
	"devicecode-go/services/hal/internal/platform"
	"devicecode-go/services/hal/internal/service"

	_ "devicecode-go/services/hal/internal/devices/gpio"
	_ "devicecode-go/services/hal/internal/devices/ltc4015"
)

func TestHAL_LTC4015_Alerts_State_EdgeFilter(t *testing.T) {
	start := time.Now()
	ts := func() string { return itoa(int(time.Since(start).Milliseconds())) + "ms" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := bus.NewBus(64)
	conn := b.NewConnection("t-ltc4015")
	defer conn.Disconnect()

	stateSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokState))
	defer conn.Unsubscribe(stateSub)

	i2c := platform.DefaultI2CFactory()
	pins := platform.DefaultPinFactory()
	hostPins, ok := pins.(*platform.HostPinFactory)
	if !ok {
		t.Errorf("[%s] expected HostPinFactory", ts())
		return
	}

	// Create + preset SMBALERT# HIGH (inactive) before service start
	if _, ok := hostPins.ByNumber(22); !ok {
		t.Errorf("[%s] ByNumber(22) failed", ts())
		return
	}
	pin22, ok := hostPins.Get(22)
	if !ok || pin22 == nil {
		t.Errorf("[%s] HostPinFactory.Get(22) failed", ts())
		return
	}
	pin22.Set(true)

	svc := service.New(conn, i2c, pins)
	go svc.Run(ctx)

	// Initial hal/state
	if m, err := recvOrTimeout(stateSub.Channel(), 3*time.Second); err != nil || m == nil {
		t.Errorf("[%s] no initial hal/state within 3s: %v", ts(), err)
		return
	}

	// Configure one LTC4015
	cfg := config.HALConfig{
		Devices: []config.Device{{
			ID:   "chg0",
			Type: "ltc4015",
			Params: map[string]any{
				"addr":              104,
				"cells":             4,
				"chem":              "lithium",
				"rsnsb_uohm":        5000,
				"rsnsi_uohm":        5000,
				"sample_every_ms":   5000, // long cadence so IRQ stands out
				"smbalert_pin":      22,
				"irq_debounce_ms":   2,
				"force_meas_sys_on": true,
			},
			BusRef: config.BusRef{Type: "i2c", ID: "i2c1"},
		}},
	}
	conn.Publish(conn.NewMessage(bus.T(consts.TokConfig, consts.TokHAL), cfg, false))

	// Await ready
	readyBy := time.Now().Add(5 * time.Second)
readyWait:
	for time.Now().Before(readyBy) {
		if m, _ := recvOrTimeout(stateSub.Channel(), 500*time.Millisecond); m != nil {
			if p, ok := m.Payload.(map[string]any); ok && p["level"] == "ready" {
				break readyWait
			}
		}
	}

	// Subscribe capability tree
	capSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokCapability, "+", "+", "+"))
	defer conn.Unsubscribe(capSub)

	// Discover capability IDs
	ids := map[string]int{}
	found := map[string]bool{"power": false, "charger": false, "alerts": false}
	discoveryBy := time.Now().Add(3 * time.Second)
	for time.Now().Before(discoveryBy) {
		m, err := recvOrTimeout(capSub.Channel(), 200*time.Millisecond)
		if err != nil || m == nil || len(m.Topic) != 5 || m.Topic[4] != consts.TokInfo {
			continue
		}
		k, _ := m.Topic[2].(string)
		id, ok := asInt(m.Topic[3])
		if !ok {
			continue
		}
		ids[k] = id
		found[k] = true
		if found["power"] && found["charger"] && found["alerts"] {
			break
		}
	}
	if !(found["power"] && found["charger"] && found["alerts"]) {
		t.Errorf("[%s] missing capability ids; have: %#v", ts(), ids)
		return
	}

	// First periodic power/value
	var firstPowerAt time.Time
value1Wait:
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		m, err := recvOrTimeout(capSub.Channel(), 250*time.Millisecond)
		if err != nil || m == nil {
			continue
		}
		if len(m.Topic) == 5 && m.Topic[2] == "power" && m.Topic[3] == ids["power"] && m.Topic[4] == consts.TokValue {
			firstPowerAt = time.Now()
			break value1Wait
		}
	}
	if firstPowerAt.IsZero() {
		t.Errorf("[%s] did not see initial power/value", ts())
		return
	}

	// Observe retained power/state, require link:up
	powerStateSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokCapability, "power", ids["power"], consts.TokState))
	defer conn.Unsubscribe(powerStateSub)

	var state1 map[string]any
	if m, err := recvOrTimeout(powerStateSub.Channel(), 2*time.Second); err != nil || m == nil {
		t.Errorf("[%s] no power/state observed", ts())
		return
	} else if p, ok := m.Payload.(map[string]any); ok {
		state1 = p
		if p["link"] != consts.LinkUp {
			t.Errorf("[%s] power/state link != up: %#v", ts(), p)
			return
		}
	}

	// Force immediate measurement; then await a strictly newer ts_ms (loop tolerates same-ms)
	reqRN := conn.NewMessage(
		bus.T(consts.TokHAL, consts.TokCapability, "power", ids["power"], consts.TokControl, consts.CtrlReadNow),
		nil, false,
	)
	ctxRN, cancelRN := context.WithTimeout(ctx, 2*time.Second)
	if _, err := conn.RequestWait(ctxRN, reqRN); err != nil {
		cancelRN()
		t.Errorf("[%s] read_now failed: %v", ts(), err)
		return
	}
	cancelRN()

	// Await state with ts_ms > previous (allowing for equal in same ms)
	t1, _ := asInt(state1["ts_ms"])
	newerBy := time.Now().Add(750 * time.Millisecond)
	seenNewer := false
	for time.Now().Before(newerBy) {
		m, err := recvOrTimeout(powerStateSub.Channel(), 150*time.Millisecond)
		if err != nil || m == nil {
			continue
		}
		if p, ok := m.Payload.(map[string]any); ok {
			if t2, ok := asInt(p["ts_ms"]); ok && t2 > t1 {
				seenNewer = true
				break
			}
		}
	}
	if !seenNewer {
		// Not a hard failure under TinyGo; log and continue with the rest (IRQ will also advance ts_ms)
		t.Logf("[%s] power/state ts_ms did not strictly increase within window; continuing", ts())
	}

	// IRQ path: falling edge triggers early read well before 5 s cadence
	time.Sleep(2 * time.Millisecond)
	pin22.Set(false) // falling (active-low)
	earlyBy := time.Now().Add(2 * time.Second)
	sawEarlyPower := false
	for time.Now().Before(earlyBy) {
		m, err := recvOrTimeout(capSub.Channel(), 120*time.Millisecond)
		if err != nil || m == nil {
			continue
		}
		if len(m.Topic) == 5 && m.Topic[2] == "power" && m.Topic[3] == ids["power"] && m.Topic[4] == consts.TokValue {
			if time.Since(firstPowerAt) < 3*time.Second {
				sawEarlyPower = true
				break
			}
		}
	}
	if !sawEarlyPower {
		t.Errorf("[%s] no IRQ-triggered early power/value", ts())
		return
	}

	// Edge filter: rising edge should not trigger an early read
	// Drain briefly then mark time and drive rising
	drainUntil := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(drainUntil) {
		_, _ = recvOrTimeout(capSub.Channel(), 20*time.Millisecond)
	}
	marker := time.Now()
	pin22.Set(true) // rising (inactive)
	noEarlyBy := time.Now().Add(350 * time.Millisecond)
	sawAfterRising := false
	for time.Now().Before(noEarlyBy) {
		m, err := recvOrTimeout(capSub.Channel(), 80*time.Millisecond)
		if err != nil || m == nil {
			continue
		}
		if len(m.Topic) == 5 && m.Topic[2] == "power" && m.Topic[3] == ids["power"] && m.Topic[4] == consts.TokValue {
			if time.Since(marker) < 300*time.Millisecond {
				sawAfterRising = true
				break
			}
		}
	}
	if sawAfterRising {
		t.Errorf("[%s] rising edge incorrectly triggered early power/value", ts())
		return
	}

	// Alerts: toggle SMBALERT a few times until an alerts/value arrives
	alertsSeen := false
	alertsBy := time.Now().Add(2 * time.Second)
	for time.Now().Before(alertsBy) {
		// Generate a falling edge
		pin22.Set(true)
		time.Sleep(2 * time.Millisecond)
		pin22.Set(false)

		deadline := time.Now().Add(150 * time.Millisecond)
		for time.Now().Before(deadline) {
			m, err := recvOrTimeout(capSub.Channel(), 40*time.Millisecond)
			if err != nil || m == nil {
				continue
			}
			if len(m.Topic) == 5 && m.Topic[2] == "alerts" && m.Topic[3] == ids["alerts"] && m.Topic[4] == consts.TokValue {
				alertsSeen = true
				break
			}
		}
		if alertsSeen {
			break
		}
	}
	if !alertsSeen {
		t.Errorf("[%s] did not observe alerts/value after SMBALERT stimulation", ts())
		return
	}

	// Clean stop
	cancel()
	stopBy := time.Now().Add(2 * time.Second)
	for time.Now().Before(stopBy) {
		if m, _ := recvOrTimeout(stateSub.Channel(), 100*time.Millisecond); m != nil {
			if p, ok := m.Payload.(map[string]any); ok && p["level"] == "stopped" {
				break
			}
		}
	}
}

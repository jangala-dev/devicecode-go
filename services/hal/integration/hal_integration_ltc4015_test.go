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

	// Ensure device builders register with the registry.
	_ "devicecode-go/services/hal/internal/devices/aht20"
	_ "devicecode-go/services/hal/internal/devices/gpio"
	_ "devicecode-go/services/hal/internal/devices/ltc4015"
)

// ------------------------- LTC4015 + SMBALERT test ---------------------------

func TestHAL_EndToEnd_LTC4015_With_SMBALERT(t *testing.T) {
	start := time.Now()
	ts := func() string { return itoa(int(time.Since(start).Milliseconds())) + "ms" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bus + connection.
	b := bus.NewBus(64)
	conn := b.NewConnection("test")
	defer conn.Disconnect()

	// Observe hal/state from the outset.
	stateSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokState))
	defer conn.Unsubscribe(stateSub)

	// Host factories.
	i2c := platform.DefaultI2CFactory()
	pins := platform.DefaultPinFactory()
	hostPins, ok := pins.(*platform.HostPinFactory)
	if !ok {
		t.Errorf("[%s] expected HostPinFactory on host build", ts())
		return
	}

	// Create pin 22 in the factory and preset HIGH before config so the IRQ
	// worker's initial snapshot is correct for an active-low SMBALERT# line.
	// HostPinFactory.Get() only returns existing pins; ensure creation via ByNumber().
	if _, ok := hostPins.ByNumber(22); !ok {
		t.Errorf("[%s] ByNumber(22) failed (cannot create fake pin)", ts())
		return
	}
	pin22, ok := hostPins.Get(22)
	if !ok || pin22 == nil {
		t.Errorf("[%s] failed to obtain host pin 22 after ByNumber()", ts())
		return
	}
	pin22.Set(true)
	t.Logf("[%s] preset pin22 HIGH (inactive SMBALERT) before config", ts())

	// Start service.
	svc := service.New(conn, i2c, pins)
	go svc.Run(ctx)

	// Initial hal/state (retained).
	if m, err := recvOrTimeout(stateSub.Channel(), 3*time.Second); err == nil {
		if p, ok := m.Payload.(map[string]any); ok {
			t.Logf("[%s] initial state: level=%v status=%v", ts(), p["level"], p["status"])
		}
	} else {
		t.Errorf("[%s] no initial hal/state within 3s: %v", ts(), err)
		return
	}

	// Configure one LTC4015 on i2c1 with SMBALERT on pin 22. Long cadence to show IRQ-triggered read.
	cfg := config.HALConfig{
		Devices: []config.Device{
			{
				ID:   "chg0",
				Type: "ltc4015",
				Params: map[string]any{
					"addr":              104, // 0x68
					"cells":             4,
					"chem":              "lithium",
					"rsnsb_uohm":        5000,
					"rsnsi_uohm":        5000,
					"sample_every_ms":   5000, // long period so IRQ stands out
					"smbalert_pin":      22,
					"irq_debounce_ms":   1,
					"force_meas_sys_on": true,
				},
				BusRef: config.BusRef{Type: "i2c", ID: "i2c1"},
			},
		},
	}
	t.Logf("[%s] publishing config for LTC4015…", ts())
	conn.Publish(conn.NewMessage(bus.T(consts.TokConfig, consts.TokHAL), cfg, false))

	// Await ready.
	readyBy := time.Now().Add(5 * time.Second)
	ready := false
	for !ready && time.Now().Before(readyBy) {
		if m, _ := recvOrTimeout(stateSub.Channel(), 500*time.Millisecond); m != nil {
			if p, ok := m.Payload.(map[string]any); ok {
				t.Logf("[%s] hal/state: %#v", ts(), p)
				if p["level"] == "ready" {
					ready = true
				}
			}
		}
	}
	if !ready {
		t.Errorf("[%s] HAL did not reach level=ready within 5s", ts())
		return
	}

	// Subscribe to capability tree.
	capSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokCapability, "+", "+", "+"))
	defer conn.Unsubscribe(capSub)

	// Discover capability IDs from retained info docs (ID 0 is valid).
	ids := map[string]int{}
	found := map[string]bool{}
	discoveryBy := time.Now().Add(3 * time.Second)
	for time.Now().Before(discoveryBy) {
		m, err := recvOrTimeout(capSub.Channel(), 250*time.Millisecond)
		if err != nil {
			continue
		}
		if len(m.Topic) != 5 || m.Topic[4] != consts.TokInfo {
			continue
		}
		kind, _ := m.Topic[2].(string)
		id, ok := asInt(m.Topic[3])
		if !ok {
			continue
		}
		ids[kind] = id
		found[kind] = true
		t.Logf("[%s] discovered %s/info with id=%d payload=%#v", ts(), kind, id, m.Payload)
		if found["power"] && found["charger"] && found["alerts"] {
			break
		}
	}
	if !found["power"] || !found["charger"] || !found["alerts"] {
		t.Errorf("[%s] missing capability ids, have: %#v", ts(), ids)
		return
	}

	// Observe the first periodic power/value.
	var firstPowerAt time.Time
	valueBy := time.Now().Add(3 * time.Second)
	for time.Now().Before(valueBy) {
		m, err := recvOrTimeout(capSub.Channel(), 300*time.Millisecond)
		if err != nil {
			continue
		}
		if len(m.Topic) == 5 && m.Topic[2] == "power" && m.Topic[3] == ids["power"] && m.Topic[4] == consts.TokValue {
			firstPowerAt = time.Now()
			t.Logf("[%s] saw initial power/value payload=%#v", ts(), m.Payload)
			break
		}
	}
	if firstPowerAt.IsZero() {
		t.Errorf("[%s] did not see initial power/value", ts())
		return
	}

	// --- IRQ path: drive SMBALERT falling edge and expect an early read ---
	time.Sleep(2 * time.Millisecond)
	t.Logf("[%s] driving pin22 LOW (simulate SMBALERT falling edge)…", ts())
	pin22.Set(false)

	irqValueBy := time.Now().Add(2 * time.Second)
	sawIRQPower := false
	for time.Now().Before(irqValueBy) {
		m, err := recvOrTimeout(capSub.Channel(), 150*time.Millisecond)
		if err != nil {
			continue
		}
		if len(m.Topic) == 5 && m.Topic[2] == "power" && m.Topic[3] == ids["power"] && m.Topic[4] == consts.TokValue {
			delta := time.Since(firstPowerAt)
			t.Logf("[%s] saw post-IRQ power/value after %v payload=%#v", ts(), delta, m.Payload)
			// Arrived well before the 5 s cadence; count it as IRQ-triggered.
			if delta < 3*time.Second {
				sawIRQPower = true
				break
			}
		}
	}
	if !sawIRQPower {
		t.Errorf("[%s] did not observe IRQ-triggered early power/value", ts())
		return
	}

	// --- Controls: set limits and verify via a forced read_now ---
	ctl := func(method string, payload map[string]any) bool {
		topic := bus.T(consts.TokHAL, consts.TokCapability, "charger", ids["charger"], consts.TokControl, method)
		req := conn.NewMessage(topic, payload, false)
		ctx2, cancel2 := context.WithTimeout(ctx, 2*time.Second)
		defer cancel2()
		t.Logf("[%s] control %s payload=%#v …", ts(), method, payload)
		reply, err := conn.RequestWait(ctx2, req)
		if err != nil {
			t.Logf("[%s] control %s failed: %v", ts(), method, err)
			return false
		}
		t.Logf("[%s] control %s reply: topic=%s payload=%#v", ts(), method, topicStr(reply.Topic), reply.Payload)
		return true
	}

	if !ctl("set_input_current_limit", map[string]any{"mA": 1500}) {
		t.Errorf("[%s] set_input_current_limit failed", ts())
		return
	}
	if !ctl("set_charge_current", map[string]any{"mA": 1200}) {
		t.Errorf("[%s] set_charge_current failed", ts())
		return
	}
	if !ctl("set_vin_uvcl", map[string]any{"mV": 4800}) {
		t.Errorf("[%s] set_vin_uvcl failed", ts())
		return
	}

	// Force an immediate measurement of the power capability so DAC reflections are visible.
	reqRN := conn.NewMessage(
		bus.T(consts.TokHAL, consts.TokCapability, "power", ids["power"], consts.TokControl, consts.CtrlReadNow),
		nil, false,
	)
	ctxRN, cancelRN := context.WithTimeout(ctx, 2*time.Second)
	defer cancelRN()
	t.Logf("[%s] issuing read_now for power capability…", ts())
	if reply, err := conn.RequestWait(ctxRN, reqRN); err != nil {
		t.Errorf("[%s] read_now failed: %v", ts(), err)
		return
	} else {
		t.Logf("[%s] read_now reply: topic=%s payload=%#v", ts(), topicStr(reply.Topic), reply.Payload)
	}

	// Verify reflected values on the next power/value.
	gotIinLim, gotIchg := false, false
	checkBy := time.Now().Add(2 * time.Second)
	for time.Now().Before(checkBy) && !(gotIinLim && gotIchg) {
		m, err := recvOrTimeout(capSub.Channel(), 250*time.Millisecond)
		if err != nil {
			continue
		}
		if len(m.Topic) == 5 && m.Topic[2] == "power" && m.Topic[3] == ids["power"] && m.Topic[4] == consts.TokValue {
			if p, ok := m.Payload.(map[string]any); ok {
				if v, ok := asInt(p["iin_limit_dac_mA"]); ok && v == 1500 {
					gotIinLim = true
				}
				if v, ok := asInt(p["icharge_dac_mA"]); ok && v == 1200 {
					gotIchg = true
				}
				t.Logf("[%s] observed power/value (post-controls): %#v (iin_limit=%v ichg=%v)", ts(), p, gotIinLim, gotIchg)
			}
		}
	}
	if !gotIinLim || !gotIchg {
		t.Errorf("[%s] controls not reflected in power/value (iin_limit=%v, ichg=%v)", ts(), gotIinLim, gotIchg)
		return
	}

	// Clean stop.
	cancel()
	stopBy := time.Now().Add(2 * time.Second)
	for time.Now().Before(stopBy) {
		if m, _ := recvOrTimeout(stateSub.Channel(), 100*time.Millisecond); m != nil {
			if p, ok := m.Payload.(map[string]any); ok && p["level"] == "stopped" {
				t.Logf("[%s] hal/state: stopped", ts())
				break
			}
		}
	}
}

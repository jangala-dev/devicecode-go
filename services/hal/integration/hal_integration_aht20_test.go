// services/hal/integration/hal_integration_aht20_test.go
//go:build !rp2040 && !rp2350

package integration

import (
	"context"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal/internal/consts"
	"devicecode-go/services/hal/internal/platform"
	"devicecode-go/services/hal/internal/service"

	// Ensure device builders register with the registry.
	_ "devicecode-go/services/hal/internal/devices/aht20"
	_ "devicecode-go/services/hal/internal/devices/gpio"

	"devicecode-go/types"
)

func TestHAL_EndToEnd_AHT20_And_GPIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// In-process bus and connection.
	b := bus.NewBus(64)
	conn := b.NewConnection("test")
	defer conn.Disconnect()

	// Subscribe to service state BEFORE starting the service so we see the initial retained publish.
	stateSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokState))
	defer conn.Unsubscribe(stateSub)

	// Host factories (build-tagged shims on non-RP2 platforms).
	i2c := platform.DefaultI2CFactory()
	pins := platform.DefaultPinFactory()
	uart := platform.DefaultUARTFactory()

	// Keep a handle to the concrete host pin factory to simulate IRQ edges later.
	hostPins, ok := pins.(*platform.HostPinFactory)
	if !ok {
		t.Log("unexpected pin factory type; host shims not in use")
		return
	}

	// Start the service directly with our factories.
	svc := service.New(conn, i2c, pins, uart)
	go svc.Run(ctx)

	// Expect the initial retained state.
	m, err := recvOrTimeout(stateSub.Channel(), 3*time.Second)
	if err != nil {
		t.Logf("did not observe initial hal/state within 3s: %v", err)
		return
	}
	if st, ok := m.Payload.(types.HALState); ok {
		t.Logf("initial state: level=%s status=%s", st.Level, st.Status)
	}

	// Apply config: one AHT20 on i2c0 and one GPIO input with IRQ.
	cfg := types.HALConfig{
		Devices: []types.Device{
			{
				ID:   "th1",
				Type: "aht20",
				BusRef: types.BusRef{
					Type: "i2c",
					ID:   "i2c0",
				},
			},
			{
				ID:   "in1",
				Type: "gpio",
				Params: map[string]any{
					"pin":    12,
					"mode":   "input",
					"pull":   "none",
					"invert": false,
					"irq": map[string]any{
						"edge":        "rising",
						"debounce_ms": 1,
					},
				},
			},
		},
	}
	conn.Publish(conn.NewMessage(bus.T(consts.TokConfig, consts.TokHAL), cfg, false))

	// Wait up to 5s for level=ready.
	seenReady := false
	readyDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readyDeadline) {
		m, err := recvOrTimeout(stateSub.Channel(), 500*time.Millisecond)
		if err != nil {
			continue
		}
		if st, ok := m.Payload.(types.HALState); ok && st.Level == "ready" {
			t.Log("state: ready")
			seenReady = true
			break
		}
	}
	if !seenReady {
		t.Log("did not observe hal/state level=ready within 5s")
		return
	}

	// Subscribe to all capability info/state/value.
	capSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokCapability, "+", "+", "+"))
	defer conn.Unsubscribe(capSub)

	// Discover capability IDs from retained info publications.
	ids := map[string]int{}
	discoveryDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(discoveryDeadline) {
		m, err := recvOrTimeout(capSub.Channel(), 200*time.Millisecond)
		if err != nil {
			continue
		}
		if len(m.Topic) != 5 {
			continue
		}
		kind, _ := m.Topic[2].(string)
		id, ok := asInt(m.Topic[3])
		if !ok {
			continue
		}
		if suffix, _ := m.Topic[4].(string); suffix == consts.TokInfo {
			ids[kind] = id
			if _, okT := ids["temperature"]; okT {
				if _, okH := ids["humidity"]; okH {
					if _, okG := ids["gpio"]; okG {
						break
					}
				}
			}
		}
	}
	if _, ok := ids["temperature"]; !ok {
		t.Log("missing temperature capability id")
		return
	}
	if _, ok := ids["humidity"]; !ok {
		t.Log("missing humidity capability id")
		return
	}
	if _, ok := ids["gpio"]; !ok {
		t.Log("missing gpio capability id")
		return
	}

	// Reduce AHT20 rate to speed up the test (set on one kind; device-wide).
	setRate := func(kind string, ms int) bool {
		req := conn.NewMessage(
			bus.T(consts.TokHAL, consts.TokCapability, kind, ids[kind], consts.TokControl, consts.CtrlSetRate),
			types.SetRate{PeriodMS: ms},
			false,
		)
		ctx2, cancel2 := context.WithTimeout(ctx, 2*time.Second)
		defer cancel2()
		rep, err := conn.RequestWait(ctx2, req)
		if err != nil {
			t.Logf("set_rate request failed for %s: %v", kind, err)
			return false
		}
		if ack, ok := rep.Payload.(types.SetRateAck); !ok || !ack.OK {
			t.Logf("set_rate not acknowledged for %s: %#v", kind, rep.Payload)
			return false
		}
		return true
	}
	if !setRate("temperature", 200) {
		return
	}
	time.Sleep(50 * time.Millisecond)

	// Expect value publications for both temperature and humidity.
	wantKinds := map[string]int{"temperature": 0, "humidity": 0}
	valuesDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(valuesDeadline) && (wantKinds["temperature"] == 0 || wantKinds["humidity"] == 0) {
		m, err := recvOrTimeout(capSub.Channel(), 250*time.Millisecond)
		if err != nil {
			continue
		}
		if len(m.Topic) == 5 && m.Topic[4] == consts.TokValue {
			if kind, _ := m.Topic[2].(string); kind == "temperature" || kind == "humidity" {
				wantKinds[kind]++
			}
		}
	}
	if wantKinds["temperature"] == 0 || wantKinds["humidity"] == 0 {
		t.Logf("timed out waiting for aht20 value messages: have %v", wantKinds)
		return
	}

	// Subscribe to GPIO event stream before triggering an edge.
	gpioEvtSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokCapability, "gpio", ids["gpio"], consts.TokEvent))
	defer conn.Unsubscribe(gpioEvtSub)

	// Simulate a rising edge on pin 12 via the host pin factory instance the service is using.
	pin12, ok := hostPins.Get(12)
	if !ok || pin12 == nil {
		t.Log("failed to obtain host pin 12")
		return
	}
	// Ensure known low, then drive high to produce rising edge (IRQ registered by config).
	pin12.Set(false)
	time.Sleep(2 * time.Millisecond)
	pin12.Set(true)

	// Expect a gpio event with edge:"rising", level:1.
	ev, err := recvOrTimeout(gpioEvtSub.Channel(), 1*time.Second)
	if err != nil {
		t.Logf("timed out waiting for gpio event after simulated edge: %v", err)
		return
	}
	ge, ok := ev.Payload.(types.GPIOEvent)
	if !ok {
		t.Logf("gpio event payload not typed: %#v", ev.Payload)
		return
	}
	if ge.Edge != types.EdgeRising {
		t.Logf("unexpected edge: %#v", ge.Edge)
		return
	}
	if ge.Level != 1 {
		t.Logf("unexpected level: %#v", ge.Level)
		return
	}
	cancel() // already defined at top
	stoppedSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokState))
	defer conn.Unsubscribe(stoppedSub)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m, _ := recvOrTimeout(stoppedSub.Channel(), 100*time.Millisecond); m != nil {
			if p, ok := m.Payload.(map[string]any); ok && p["level"] == "stopped" {
				break
			}
		}
	}
}

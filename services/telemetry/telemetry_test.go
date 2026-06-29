package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/types"
)

func newTestBus() *bus.Bus { return bus.NewBus(8, "+", "#") }

// runService is the same kind of subscribe-then-start helper used in
// services/updater_test.go: a fresh probe subscription on a bus
// connection guarantees we capture the first publish without racing
// the goroutine's Subscribe calls.
func runService(t *testing.T, b *bus.Bus) (*bus.Connection, context.CancelFunc) {
	t.Helper()
	return runServiceWithPolicy(t, b, DefaultPolicy)
}

func runServiceWithPolicy(t *testing.T, b *bus.Bus, policy Policy) (*bus.Connection, context.CancelFunc) {
	t.Helper()
	conn := b.NewConnection("telemetry")
	svc := NewWithPolicy(conn, policy)
	ctx, cancel := context.WithCancel(context.Background())
	go svc.Run(ctx)
	// Telemetry only emits in response to incoming HAL data, so we
	// don't need to wait on a startup retain; the SubscribeOnHAL test
	// below uses a settle delay.
	time.Sleep(10 * time.Millisecond)
	return conn, cancel
}

func publishChargerPresent(hal *bus.Connection) {
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{
		State: 0,
		Sys:   uint16(types.OkToCharge),
	}, true))
}

func TestPublishesBatteryFact(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicBattery)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	hal := b.NewConnection("hal")
	publishChargerPresent(hal)
	hal.Publish(hal.NewMessage(halPwrAny, types.BatteryValue{
		PackMilliV:      12000,
		PerCellMilliV:   3000,
		IBatMilliA:      -500,
		TempMilliC:      24500,
		BSR_uOhmPerCell: 1200,
	}, true))

	fact := waitForMeasuredBatteryFact(t, sub)
	if fact.Presence != "present" || !fact.MeasurementsValid {
		t.Fatalf("battery presence wrong: %+v", fact)
	}
	if fact.PackMV != 12000 || fact.IBatMA != -500 || fact.BSRUOhmPerCell != 1200 {
		t.Fatalf("battery fact wrong: %+v", fact)
	}
	if fact.Seq == 0 {
		t.Fatalf("seq = %d, want > 0", fact.Seq)
	}
	if fact.UptimeMs < 0 {
		t.Fatalf("uptime_ms = %d, want >= 0", fact.UptimeMs)
	}
}

func TestPublishesChargerWithBitfieldsOnly(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicCharger)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	hal := b.NewConnection("hal")
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{
		VIN_mV:  18000,
		VSYS_mV: 12200,
		IIn_mA:  500,
		State:   uint16(types.AbsorbCharge | types.CCCVCharge),
		Status:  uint16(types.IinLimitActive),
		Sys:     uint16(types.ChargerEnabled | types.OkToCharge),
	}, true))

	select {
	case msg := <-sub.Channel():
		fact, ok := msg.Payload.(ChargerFact)
		if !ok {
			t.Fatalf("payload type = %T", msg.Payload)
		}
		if fact.VinMV != 18000 || fact.VsysMV != 12200 || fact.IinMA != 500 {
			t.Fatalf("analog values wrong: %+v", fact)
		}
		if fact.StateBits != uint16(types.AbsorbCharge|types.CCCVCharge) {
			t.Fatalf("state_bits = 0x%x", fact.StateBits)
		}
		if fact.StatusBits != uint16(types.IinLimitActive) {
			t.Fatalf("status_bits = 0x%x", fact.StatusBits)
		}
		if fact.SystemBits != uint16(types.ChargerEnabled|types.OkToCharge) {
			t.Fatalf("system_bits = 0x%x", fact.SystemBits)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for charger fact")
	}
}

func TestPublishesEnvironmentFacts(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	tSub := observer.Subscribe(TopicEnvTemp)
	defer observer.Unsubscribe(tSub)
	hSub := observer.Subscribe(TopicEnvHumidity)
	defer observer.Unsubscribe(hSub)

	_, cancel := runService(t, b)
	defer cancel()

	hal := b.NewConnection("hal")
	hal.Publish(hal.NewMessage(halEnvTemp, types.TemperatureValue{DeciC: 235}, true))
	hal.Publish(hal.NewMessage(halEnvHum, types.HumidityValue{RHx100: 4530}, true))

	select {
	case msg := <-tSub.Channel():
		fact, ok := msg.Payload.(EnvTempFact)
		if !ok || fact.DeciC != 235 {
			t.Fatalf("env temp fact = %+v ok=%v", msg.Payload, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for env temp fact")
	}
	select {
	case msg := <-hSub.Channel():
		fact, ok := msg.Payload.(EnvHumFact)
		if !ok || fact.RHx100 != 4530 {
			t.Fatalf("env hum fact = %+v ok=%v", msg.Payload, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for env hum fact")
	}
}

func TestSuppressesInsignificantBatteryChanges(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicBattery)
	defer observer.Unsubscribe(sub)

	policy := DefaultPolicy
	policy.KeepaliveInterval = time.Hour

	_, cancel := runServiceWithPolicy(t, b, policy)
	defer cancel()

	hal := b.NewConnection("hal")
	publishChargerPresent(hal)
	baseline := types.BatteryValue{
		PackMilliV:      12000,
		PerCellMilliV:   3000,
		IBatMilliA:      -500,
		TempMilliC:      24500,
		BSR_uOhmPerCell: 1200,
	}
	hal.Publish(hal.NewMessage(halPwrAny, baseline, true))
	first := waitForMeasuredBatteryFact(t, sub)
	if first.Seq == 0 {
		t.Fatalf("first seq = %d, want > 0", first.Seq)
	}

	// All fields move less than their configured absolute/percentage
	// thresholds, so the retained state should not be republished.
	noisy := baseline
	noisy.PackMilliV += 50
	noisy.PerCellMilliV += 10
	noisy.IBatMilliA -= 25
	noisy.TempMilliC += 100
	noisy.BSR_uOhmPerCell += 100
	hal.Publish(hal.NewMessage(halPwrAny, noisy, true))
	assertNoMessage(t, sub, 150*time.Millisecond, "insignificant battery change")

	// A meaningful pack-voltage movement crosses the configured 100 mV
	// threshold and should publish with the next sequence number.
	changed := baseline
	changed.PackMilliV += 100
	hal.Publish(hal.NewMessage(halPwrAny, changed, true))
	second := waitForBatteryFact(t, sub)
	if second.Seq != first.Seq+1 || second.PackMV != changed.PackMilliV {
		t.Fatalf("second battery fact = %+v, want seq=%d pack=%d", second, first.Seq+1, changed.PackMilliV)
	}
}

func TestSuppressesNearZeroBatteryCurrentJitter(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicBattery)
	defer observer.Unsubscribe(sub)

	policy := DefaultPolicy
	policy.KeepaliveInterval = time.Hour

	_, cancel := runServiceWithPolicy(t, b, policy)
	defer cancel()

	hal := b.NewConnection("hal")
	publishChargerPresent(hal)
	baseline := types.BatteryValue{
		PackMilliV:    13188,
		PerCellMilliV: 2198,
		IBatMilliA:    7,
		TempMilliC:    38881,
	}
	hal.Publish(hal.NewMessage(halPwrAny, baseline, true))
	first := waitForMeasuredBatteryFact(t, sub)
	if first.Seq == 0 {
		t.Fatalf("first seq = %d, want > 0", first.Seq)
	}

	// Around zero, a 1mA change is a large percentage but not a useful
	// telemetry change. Battery current uses an absolute threshold only.
	jitter := baseline
	jitter.IBatMilliA = 6
	hal.Publish(hal.NewMessage(halPwrAny, jitter, true))
	assertNoMessage(t, sub, 150*time.Millisecond, "near-zero battery current jitter")
}

func TestBatteryKeepaliveRepublishesUnchangedValue(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicBattery)
	defer observer.Unsubscribe(sub)

	policy := DefaultPolicy
	policy.KeepaliveInterval = 50 * time.Millisecond

	_, cancel := runServiceWithPolicy(t, b, policy)
	defer cancel()

	hal := b.NewConnection("hal")
	publishChargerPresent(hal)
	v := types.BatteryValue{PackMilliV: 12000, PerCellMilliV: 3000}
	hal.Publish(hal.NewMessage(halPwrAny, v, true))
	first := waitForMeasuredBatteryFact(t, sub)
	if first.Seq == 0 {
		t.Fatalf("first seq = %d, want > 0", first.Seq)
	}

	time.Sleep(75 * time.Millisecond)
	hal.Publish(hal.NewMessage(halPwrAny, v, true))
	second := waitForBatteryFact(t, sub)
	if second.Seq != first.Seq+1 || second.PackMV != v.PackMilliV {
		t.Fatalf("keepalive battery fact = %+v, want seq=%d pack=%d", second, first.Seq+1, v.PackMilliV)
	}
}

func TestChargerPresentPublishesBatteryPresenceBeforeMeasurement(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicBattery)
	defer observer.Unsubscribe(sub)

	policy := DefaultPolicy
	policy.KeepaliveInterval = time.Hour

	_, cancel := runServiceWithPolicy(t, b, policy)
	defer cancel()

	hal := b.NewConnection("hal")
	publishChargerPresent(hal)

	fact := waitForBatteryFact(t, sub)
	if fact.Presence != "present" || fact.MeasurementsValid || fact.Reason != "battery_measurement_unavailable" {
		t.Fatalf("battery present-before-measurement fact = %+v", fact)
	}
	v := types.BatteryValue{PackMilliV: 13008, PerCellMilliV: 2168, IBatMilliA: 7, TempMilliC: 37763}
	hal.Publish(hal.NewMessage(halPwrAny, v, true))
	present := waitForBatteryFact(t, sub)
	if present.Presence != "present" || !present.MeasurementsValid || present.Reason != "" {
		t.Fatalf("battery present-after-measurement fact = %+v", present)
	}
	if present.PackMV != v.PackMilliV {
		t.Fatalf("present battery pack_mV = %+v, want %d", present, v.PackMilliV)
	}
}

func TestBatteryMissingFaultPublishesAbsenceWithoutMeasurements(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicBattery)
	defer observer.Unsubscribe(sub)

	policy := DefaultPolicy
	policy.KeepaliveInterval = time.Hour

	_, cancel := runServiceWithPolicy(t, b, policy)
	defer cancel()

	hal := b.NewConnection("hal")
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{
		State: uint16(types.BatMissingFault),
	}, true))

	fact := waitForBatteryFact(t, sub)
	if fact.Presence != "absent" || fact.MeasurementsValid || fact.Reason != "bat_missing_fault" {
		t.Fatalf("battery absence fact = %+v", fact)
	}
	wire, err := json.Marshal(fact)
	if err != nil {
		t.Fatalf("marshal absent battery fact: %v", err)
	}
	if strings.Contains(string(wire), "pack_mV") || strings.Contains(string(wire), "ibat_mA") {
		t.Fatalf("absent battery wire payload included analogue fields: %s", string(wire))
	}

	// Large floating-sense readings while bat_missing_fault remains set are
	// not meaningful battery telemetry and should not republish the retained
	// battery fact before the liveness interval.
	hal.Publish(hal.NewMessage(halPwrAny, types.BatteryValue{
		PackMilliV:    6,
		PerCellMilliV: 1,
		IBatMilliA:    -6,
		TempMilliC:    37960,
	}, true))
	hal.Publish(hal.NewMessage(halPwrAny, types.BatteryValue{
		PackMilliV:    13008,
		PerCellMilliV: 2168,
		IBatMilliA:    7,
		TempMilliC:    38815,
	}, true))
	assertNoMessage(t, sub, 150*time.Millisecond, "noisy absent battery measurements")
}

func TestBatteryPresenceReturningPublishesMeasurements(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicBattery)
	defer observer.Unsubscribe(sub)

	policy := DefaultPolicy
	policy.KeepaliveInterval = time.Hour

	_, cancel := runServiceWithPolicy(t, b, policy)
	defer cancel()

	hal := b.NewConnection("hal")
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{
		State: uint16(types.BatMissingFault),
	}, true))
	absent := waitForBatteryFact(t, sub)
	if absent.Presence != "absent" {
		t.Fatalf("initial presence = %+v, want absent", absent)
	}

	v := types.BatteryValue{PackMilliV: 13008, PerCellMilliV: 2168, IBatMilliA: 7, TempMilliC: 37763}
	hal.Publish(hal.NewMessage(halPwrAny, v, true))
	assertNoMessage(t, sub, 150*time.Millisecond, "battery measurement while absent")

	publishChargerPresent(hal)
	present := waitForBatteryFact(t, sub)
	if present.Presence != "present" || !present.MeasurementsValid {
		t.Fatalf("presence after charger clears fault = %+v", present)
	}
	if present.PackMV != v.PackMilliV {
		t.Fatalf("present battery pack_mV = %+v, want %d", present, v.PackMilliV)
	}
}

func TestChargerBitfieldChangePublishesDespiteSmallAnalogueDrift(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicCharger)
	defer observer.Unsubscribe(sub)

	policy := DefaultPolicy
	policy.KeepaliveInterval = time.Hour

	_, cancel := runServiceWithPolicy(t, b, policy)
	defer cancel()

	hal := b.NewConnection("hal")
	baseline := types.ChargerValue{VIN_mV: 12000, VSYS_mV: 12100, IIn_mA: 300}
	hal.Publish(hal.NewMessage(halPwrAny, baseline, true))
	first := waitForChargerFact(t, sub)
	if first.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", first.Seq)
	}

	changed := baseline
	changed.VIN_mV += 25
	changed.Status = uint16(types.IinLimitActive)
	hal.Publish(hal.NewMessage(halPwrAny, changed, true))
	second := waitForChargerFact(t, sub)
	if second.Seq != 2 || second.StatusBits != uint16(types.IinLimitActive) {
		t.Fatalf("charger bitfield fact = %+v", second)
	}
}

func TestAlertFSMStillRunsWhenRetainedBatteryFactSuppressed(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	alertSub := observer.Subscribe(TopicChargerAlert)
	defer observer.Unsubscribe(alertSub)
	batterySub := observer.Subscribe(TopicBattery)
	defer observer.Unsubscribe(batterySub)

	policy := DefaultPolicy
	policy.KeepaliveInterval = time.Hour
	policy.BatteryBSRMinDeltaUOhmPerCell = 1000
	policy.BatteryBSRMinDeltaPct = 0

	_, cancel := runServiceWithPolicy(t, b, policy)
	defer cancel()

	hal := b.NewConnection("hal")
	publishChargerPresent(hal)
	hal.Publish(hal.NewMessage(halPwrAny, types.BatteryValue{BSR_uOhmPerCell: 4999}, true))
	_ = waitForMeasuredBatteryFact(t, batterySub)

	// Crosses the alert threshold, but not the retained telemetry delta
	// threshold. The sparse event must still fire.
	hal.Publish(hal.NewMessage(halPwrAny, types.BatteryValue{BSR_uOhmPerCell: 5001}, true))
	assertNoMessage(t, batterySub, 150*time.Millisecond, "suppressed BSR retained fact")

	select {
	case msg := <-alertSub.Channel():
		ev, ok := msg.Payload.(AlertEvent)
		if !ok || ev.Kind != AlertBsrHigh {
			t.Fatalf("alert = %+v ok=%v, want bsr_high", msg.Payload, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for bsr_high alert")
	}
}

func TestAllAlertKindsCount(t *testing.T) {
	if got := len(AllAlertKinds); got != 14 {
		t.Fatalf("AllAlertKinds has %d entries, want 14", got)
	}
	// Spec-frozen names — typo in the kind enum is a wire-break, so
	// guard the canonical strings explicitly.
	want := []string{
		"vin_lo", "vin_hi", "bsr_high",
		"bat_missing", "bat_short", "max_charge_time_fault",
		"absorb", "equalize", "cccv", "precharge",
		"iin_limited", "uvcl_active", "cc_phase", "cv_phase",
	}
	for i, k := range AllAlertKinds {
		if string(k) != want[i] {
			t.Fatalf("AllAlertKinds[%d] = %q, want %q", i, string(k), want[i])
		}
	}
}

func TestChargerAlertFSMEdgeOnly(t *testing.T) {
	// Spec: "On bit-set transition for a kind, emit one normal event."
	// Subsequent retains that keep the bit set must NOT re-emit.
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicChargerAlert)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	hal := b.NewConnection("hal")
	// First publish primes the FSM (no alerts emitted on init).
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{}, true))
	time.Sleep(20 * time.Millisecond)

	// Bit goes from clear to set: one alert emitted.
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{
		Status: uint16(types.IinLimitActive),
	}, true))

	select {
	case msg := <-sub.Channel():
		ev, ok := msg.Payload.(AlertEvent)
		if !ok {
			t.Fatalf("payload type = %T", msg.Payload)
		}
		if ev.Kind != AlertIinLimited {
			t.Fatalf("kind = %q, want %q", ev.Kind, AlertIinLimited)
		}
		if ev.Source != "ltc4015" {
			t.Fatalf("source = %q", ev.Source)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for first alert")
	}

	// Second publish keeps the bit set — no new alert.
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{
		Status: uint16(types.IinLimitActive),
	}, true))
	select {
	case msg := <-sub.Channel():
		t.Fatalf("unexpected duplicate alert: %+v", msg.Payload)
	case <-time.After(150 * time.Millisecond):
	}

	// Bit clears, then sets again: one more alert.
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{}, true))
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{
		Status: uint16(types.IinLimitActive),
	}, true))

	select {
	case msg := <-sub.Channel():
		ev, _ := msg.Payload.(AlertEvent)
		if ev.Kind != AlertIinLimited {
			t.Fatalf("re-edge alert kind = %q", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for re-edge alert")
	}
}

func TestPublishesChargerConfigAtStartup(t *testing.T) {
	// state/self/power/charger/config retains at startup with the
	// conservative defaults from DefaultChargerConfig().
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicChargerCfg)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	select {
	case msg := <-sub.Channel():
		fact, ok := msg.Payload.(ChargerConfigFact)
		if !ok {
			t.Fatalf("payload type = %T", msg.Payload)
		}
		if fact.Schema != "charger-config/1" || fact.Source != "ltc4015-default" {
			t.Fatalf("schema/source wrong: %+v", fact)
		}
		if fact.Thresholds.VinLoMV == 0 || fact.Thresholds.VinHiMV == 0 || fact.Thresholds.BSRHighUohmPerCell == 0 {
			t.Fatalf("default thresholds not populated: %+v", fact.Thresholds)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for charger config fact")
	}
}

func TestFabricLinkStateDoesNotRepublishChargerConfig(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicChargerCfg)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	// Drain startup config retain. Fabric, not telemetry, owns retained-state
	// replay when a CM5 link becomes ready.
	waitForChargerConfig(t, sub)

	publisher := b.NewConnection("test-fabric")
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": true, "peer_sid": "cm5-a", "local_sid": "mcu-a"},
		true,
	))
	assertNoChargerConfig(t, sub, 150*time.Millisecond)

	publisher.Publish(publisher.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": true, "peer_sid": "cm5-b", "local_sid": "mcu-a"},
		true,
	))
	assertNoChargerConfig(t, sub, 150*time.Millisecond)
}

func waitForBatteryFact(t *testing.T, sub *bus.Subscription) BatteryFact {
	t.Helper()
	select {
	case msg := <-sub.Channel():
		fact, ok := msg.Payload.(BatteryFact)
		if !ok {
			t.Fatalf("payload type = %T", msg.Payload)
		}
		return fact
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for battery fact")
	}
	return BatteryFact{}
}

func waitForMeasuredBatteryFact(t *testing.T, sub *bus.Subscription) BatteryFact {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sub.Channel():
			fact, ok := msg.Payload.(BatteryFact)
			if !ok {
				t.Fatalf("payload type = %T", msg.Payload)
			}
			if fact.MeasurementsValid {
				return fact
			}
		case <-deadline:
			t.Fatal("timeout waiting for measured battery fact")
		}
	}
}

func waitForChargerFact(t *testing.T, sub *bus.Subscription) ChargerFact {
	t.Helper()
	select {
	case msg := <-sub.Channel():
		fact, ok := msg.Payload.(ChargerFact)
		if !ok {
			t.Fatalf("payload type = %T", msg.Payload)
		}
		return fact
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for charger fact")
	}
	return ChargerFact{}
}

func assertNoMessage(t *testing.T, sub *bus.Subscription, d time.Duration, context string) {
	t.Helper()
	settled := time.After(d)
	for {
		select {
		case msg := <-sub.Channel():
			t.Fatalf("unexpected publish for %s: %+v", context, msg.Payload)
		case <-settled:
			return
		}
	}
}

func waitForChargerConfig(t *testing.T, sub *bus.Subscription) {
	t.Helper()
	select {
	case msg := <-sub.Channel():
		if _, ok := msg.Payload.(ChargerConfigFact); !ok {
			t.Fatalf("payload type = %T", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for charger config fact")
	}
}

func assertNoChargerConfig(t *testing.T, sub *bus.Subscription, d time.Duration) {
	t.Helper()
	settled := time.After(d)
	for {
		select {
		case <-sub.Channel():
			t.Fatal("unexpected charger config republish on unchanged Ready=true retain")
		case <-settled:
			return
		}
	}
}

func TestChargerAlertFSMVinLoEdge(t *testing.T) {
	// vin_lo fires on ChargerValue.VIN_mV crossing below the configured
	// threshold. Subsequent observations below the threshold do NOT re-fire.
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicChargerAlert)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	hal := b.NewConnection("hal")
	// Prime the FSM with vin above threshold (default vin_lo = 10500).
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{VIN_mV: 12000}, true))
	time.Sleep(20 * time.Millisecond)

	// vin drops below threshold.
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{VIN_mV: 10000}, true))

	select {
	case msg := <-sub.Channel():
		ev, _ := msg.Payload.(AlertEvent)
		if ev.Kind != AlertVinLo {
			t.Fatalf("kind = %q, want vin_lo", ev.Kind)
		}
		if ev.Severity != "warning" {
			t.Fatalf("severity = %q, want warning", ev.Severity)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for vin_lo alert")
	}

	// Stays below — no re-emit.
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{VIN_mV: 9500}, true))
	select {
	case msg := <-sub.Channel():
		t.Fatalf("unexpected duplicate vin_lo: %+v", msg.Payload)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestChargerAlertFSMVinHiEdge(t *testing.T) {
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicChargerAlert)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	hal := b.NewConnection("hal")
	// Default vin_hi = 17000; prime below threshold.
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{VIN_mV: 12000}, true))
	time.Sleep(20 * time.Millisecond)
	// Cross above.
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{VIN_mV: 18000}, true))

	select {
	case msg := <-sub.Channel():
		ev, _ := msg.Payload.(AlertEvent)
		if ev.Kind != AlertVinHi {
			t.Fatalf("kind = %q, want vin_hi", ev.Kind)
		}
		if ev.Severity != "warning" {
			t.Fatalf("severity = %q, want warning", ev.Severity)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for vin_hi alert")
	}
}

func TestChargerAlertFSMBSRHighEdge(t *testing.T) {
	// bsr_high observes BatteryValue.BSR_uOhmPerCell against the
	// threshold from charger config (default 5000 uohm/cell).
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicChargerAlert)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	hal := b.NewConnection("hal")
	// Prime with healthy BSR (below threshold).
	hal.Publish(hal.NewMessage(halPwrAny, types.BatteryValue{BSR_uOhmPerCell: 2000}, true))
	time.Sleep(20 * time.Millisecond)
	// Crosses threshold.
	hal.Publish(hal.NewMessage(halPwrAny, types.BatteryValue{BSR_uOhmPerCell: 6000}, true))

	select {
	case msg := <-sub.Channel():
		ev, _ := msg.Payload.(AlertEvent)
		if ev.Kind != AlertBsrHigh {
			t.Fatalf("kind = %q, want bsr_high", ev.Kind)
		}
		if ev.Severity != "warning" {
			t.Fatalf("severity = %q, want warning", ev.Severity)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for bsr_high alert")
	}
}

func TestChargerAlertFSMMultipleBitsTransitionTogether(t *testing.T) {
	// Two state bits flip in the same publish — both alerts should
	// fire. The order is deterministic per the FSM's switch order.
	b := newTestBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(TopicChargerAlert)
	defer observer.Unsubscribe(sub)

	_, cancel := runService(t, b)
	defer cancel()

	hal := b.NewConnection("hal")
	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{}, true))
	time.Sleep(20 * time.Millisecond)

	hal.Publish(hal.NewMessage(halPwrAny, types.ChargerValue{
		State: uint16(types.AbsorbCharge | types.CCCVCharge),
	}, true))

	gotKinds := make(map[AlertKind]bool)
	deadline := time.After(2 * time.Second)
	for len(gotKinds) < 2 {
		select {
		case msg := <-sub.Channel():
			ev, ok := msg.Payload.(AlertEvent)
			if !ok {
				continue
			}
			gotKinds[ev.Kind] = true
		case <-deadline:
			t.Fatalf("only got %v before deadline; want absorb + cccv", gotKinds)
		}
	}
	if !gotKinds[AlertAbsorb] || !gotKinds[AlertCccv] {
		t.Fatalf("expected absorb+cccv, got %v", gotKinds)
	}
}

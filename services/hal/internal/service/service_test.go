package service

import (
	"context"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal/internal/consts"
	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/types"

	"tinygo.org/x/drivers"
)

// ---- Test fakes ----

type nopBusFactory struct{}

func (nopBusFactory) ByID(id string) (drivers.I2C, bool) { return nil, false }

type nopPinFactory struct{}

func (nopPinFactory) ByNumber(int) (halcore.GPIOPin, bool) { return nil, false }

// no-op UART factory to satisfy Service.New
type nopUARTFactory struct{}

func (nopUARTFactory) ByID(id string) (halcore.UARTPort, bool) { return nil, false }

// ---- Test device/adaptor & builder ----

type svcTestAdaptor struct {
	id string
}

func (a *svcTestAdaptor) ID() string { return a.id }
func (a *svcTestAdaptor) Capabilities() []halcore.CapInfo {
	return []halcore.CapInfo{{Kind: "temp", Info: map[string]any{"unit": "C"}}}
}
func (a *svcTestAdaptor) Trigger(ctx context.Context) (time.Duration, error) {
	return 5 * time.Millisecond, nil
}
func (a *svcTestAdaptor) Collect(ctx context.Context) (halcore.Sample, error) {
	return halcore.Sample{
		{Kind: "temp", Payload: types.IntValue{Value: 42, TS: time.Now()}},
	}, nil
}
func (a *svcTestAdaptor) Control(kind, method string, payload any) (any, error) {
	return struct{ OK bool }{OK: true}, nil
}

type svcBuilder struct{}

func (svcBuilder) Build(in registry.BuildInput) (registry.BuildOutput, error) {
	return registry.BuildOutput{
		Adaptor:     &svcTestAdaptor{id: in.DeviceID},
		BusID:       "bus0",
		SampleEvery: 50 * time.Millisecond,
	}, nil
}

func ensureRegistered(t *testing.T, typ string, b registry.Builder) {
	t.Helper()
	if _, ok := registry.Lookup(typ); !ok {
		registry.RegisterBuilder(typ, b)
	}
}

// Build-and-subscribe helper: constructs a topic via bus.T(...)
func subT(t *testing.T, c *bus.Connection, tokens ...bus.Token) *bus.Subscription {
	t.Helper()
	return c.Subscribe(bus.T(tokens...))
}

func recvWithin[T any](t *testing.T, ch <-chan T, d time.Duration) (T, bool) {
	t.Helper()
	var zero T
	select {
	case v := <-ch:
		return v, true
	case <-time.After(d):
		return zero, false
	}
}

// ---- Tests ----

func TestServicePublishesStateAndValues(t *testing.T) {
	ensureRegistered(t, "svc_testdev", svcBuilder{})

	b := bus.NewBus(8)
	conn := b.NewConnection("test")

	s := New(conn, nopBusFactory{}, nopPinFactory{}, nopUARTFactory{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Subscribe to retained service state.
	stateSub := subT(t, conn, consts.TokHAL, consts.TokState)
	defer conn.Unsubscribe(stateSub)

	// Expect initial idle.
	if msg, ok := recvWithin(t, stateSub.Channel(), 200*time.Millisecond); ok {
		st, ok := msg.Payload.(types.HALState)
		if !ok {
			t.Fatalf("unexpected hal/state payload type: %T", msg.Payload)
		}
		if st.Level != "idle" {
			t.Fatalf("expected initial idle, got %+v", st)
		}
	} else {
		t.Fatal("did not receive initial hal/state")
	}

	// Apply config with single device of our test type.
	cfg := types.HALConfig{
		Devices: []types.Device{{ID: "d1", Type: "svc_testdev"}},
	}
	conn.Publish(conn.NewMessage(bus.T(consts.TokConfig, consts.TokHAL), cfg, false))

	// State should move to ready.
	if msg, ok := recvWithin(t, stateSub.Channel(), 500*time.Millisecond); ok {
		st, ok := msg.Payload.(types.HALState)
		if !ok {
			t.Fatalf("unexpected hal/state payload type: %T", msg.Payload)
		}
		if st.Level != "ready" {
			t.Fatalf("expected ready, got %+v", st)
		}
	} else {
		t.Fatal("did not receive ready hal/state")
	}

	// Subscribe to values and state for capability temp/0.
	valSub := subT(t, conn, consts.TokHAL, consts.TokCapability, "temp", 0, consts.TokValue)
	defer conn.Unsubscribe(valSub)
	stSub := subT(t, conn, consts.TokHAL, consts.TokCapability, "temp", 0, consts.TokState)
	defer conn.Unsubscribe(stSub)

	// Expect at least one value within a short interval.
	if msg, ok := recvWithin(t, valSub.Channel(), 1*time.Second); !ok {
		t.Fatal("timeout waiting for value")
	} else {
		if msg.Payload == nil {
			t.Fatalf("unexpected nil payload")
		}
	}

	// State retained should be 'up'.
	if msg, ok := recvWithin(t, stSub.Channel(), 500*time.Millisecond); ok {
		cs, ok := msg.Payload.(types.CapabilityState)
		if !ok {
			t.Fatalf("unexpected capability state payload: %T", msg.Payload)
		}
		if cs.Link != types.LinkUp {
			t.Fatalf("expected link up, got %+v", cs)
		}
	} else {
		t.Fatal("timeout waiting for retained state")
	}

	// Exercise control plane: read_now and set_rate.
	req := conn.NewMessage(
		bus.T(consts.TokHAL, consts.TokCapability, "temp", 0, consts.TokControl, consts.CtrlReadNow),
		nil, false,
	)
	ctxReq, cancelReq := context.WithTimeout(context.Background(), time.Second)
	defer cancelReq()
	reply, err := conn.RequestWait(ctxReq, req)
	if err != nil {
		t.Fatalf("read_now request failed: %v", err)
	}
	_ = reply

	// Change rate.
	req2 := conn.NewMessage(
		bus.T(consts.TokHAL, consts.TokCapability, "temp", 0, consts.TokControl, consts.CtrlSetRate),
		types.SetRate{Period: 200 * time.Millisecond},
		false,
	)
	ctxReq2, cancelReq2 := context.WithTimeout(context.Background(), time.Second)
	defer cancelReq2()
	reply2, err := conn.RequestWait(ctxReq2, req2)
	if err != nil {
		t.Fatalf("set_rate request failed: %v", err)
	}
	_ = reply2
}

func TestServiceApplyConfigRemovesDevices(t *testing.T) {
	ensureRegistered(t, "svc_testdev2", svcBuilder{})

	b := bus.NewBus(8)
	conn := b.NewConnection("test2")

	s := New(conn, nopBusFactory{}, nopPinFactory{}, nopUARTFactory{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// 1) Prove the service is up and subscribed by waiting for the retained "idle" state.
	waitHALLevel := func(level string, timeout time.Duration) {
		sub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokState))
		defer conn.Unsubscribe(sub)
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			select {
			case msg := <-sub.Channel():
				if st, ok := msg.Payload.(types.HALState); ok {
					if st.Level == level {
						return
					}
				}
			case <-time.After(25 * time.Millisecond):
			}
		}
		t.Fatalf("timeout waiting for hal/state level=%q", level)
	}

	waitHALLevel("idle", 1*time.Second)

	// 2) Subscribe to capability state (wildcard id) before applying config.
	stSub := conn.Subscribe(bus.T(consts.TokHAL, consts.TokCapability, "temp", "+", consts.TokState))
	defer conn.Unsubscribe(stSub)

	// Helper to wait for a specified link value on any temp capability.
	waitCapLink := func(want types.LinkState, timeout time.Duration) (any, bool) {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			select {
			case msg := <-stSub.Channel():
				if cs, ok := msg.Payload.(types.CapabilityState); ok {
					if cs.Link == want {
						if len(msg.Topic) >= 5 {
							return msg.Topic[3], true // capability id token
						}
						return nil, true
					}
				}
			case <-time.After(50 * time.Millisecond):
			}
		}
		return nil, false
	}

	// Apply config with one device and expect link=up.
	conn.Publish(conn.NewMessage(
		bus.T(consts.TokConfig, consts.TokHAL),
		types.HALConfig{Devices: []types.Device{{ID: "dX", Type: "svc_testdev2"}}},
		false,
	))
	idTok, ok := waitCapLink(types.LinkUp, 2*time.Second)
	if !ok {
		t.Errorf("timeout waiting for initial capability state link=up")
		return
	}

	// Reconfigure with no devices and expect link=down for the same id.
	conn.Publish(conn.NewMessage(
		bus.T(consts.TokConfig, consts.TokHAL),
		types.HALConfig{Devices: nil},
		false,
	))

	downOK := false
	deadline := time.Now().Add(2 * time.Second)
outer:
	for time.Now().Before(deadline) {
		select {
		case msg := <-stSub.Channel():
			if len(msg.Topic) < 5 || msg.Topic[3] != idTok {
				continue
			}
			if cs, ok := msg.Payload.(types.CapabilityState); ok {
				if cs.Link == types.LinkDown {
					downOK = true
					break outer
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !downOK {
		t.Errorf("timeout waiting for capability id %v link=down after removal", idTok)
	}
}

package fabric

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/types"
)

func TestBridgeMapsConfigDeviceToConfigHAL(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridgeConn := b.NewConnection("fabric-bridge")
	go RunBridge(ctx, bridgeConn)

	writer := b.NewConnection("writer")
	reader := b.NewConnection("reader")
	sub := reader.Subscribe(bus.T("config", "hal"))
	defer reader.Unsubscribe(sub)

	in := types.HALConfig{
		Devices: []types.HALDevice{
			{ID: "led0", Type: "gpio_led"},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	writer.Publish(writer.NewMessage(bus.T("config", "device"), json.RawMessage(raw), true))

	select {
	case msg := <-sub.Channel():
		if msg == nil {
			t.Fatal("config/hal subscription closed")
		}
		out, ok := msg.Payload.(types.HALConfig)
		if !ok {
			t.Fatalf("payload type = %T, want types.HALConfig", msg.Payload)
		}
		if len(out.Devices) != 1 || out.Devices[0].ID != "led0" {
			t.Fatalf("unexpected config: %+v", out)
		}
		if !msg.Retained {
			t.Fatal("config/hal message was not retained")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for config/hal")
	}
}

func TestBridgeIgnoresNonHALConfigObject(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridgeConn := b.NewConnection("fabric-bridge")
	go RunBridge(ctx, bridgeConn)

	writer := b.NewConnection("writer")
	reader := b.NewConnection("reader")
	sub := reader.Subscribe(bus.T("config", "hal"))
	defer reader.Unsubscribe(sub)

	writer.Publish(writer.NewMessage(bus.T("config", "device"), json.RawMessage(`{"source":"monitor_auto_probe"}`), true))

	select {
	case msg := <-sub.Channel():
		t.Fatalf("unexpected config/hal message: %+v", msg)
	case <-time.After(150 * time.Millisecond):
	}

	req := b.NewConnection("requester")
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer reqCancel()

	replyMsg, err := req.RequestWait(reqCtx, req.NewMessage(
		bus.T("rpc", "hal", "dump"),
		json.RawMessage(`{"ask":"status"}`),
		false,
	))
	if err != nil {
		t.Fatalf("RequestWait: %v", err)
	}

	reply, ok := replyMsg.Payload.(dumpReply)
	if !ok {
		t.Fatalf("reply payload type = %T, want dumpReply", replyMsg.Payload)
	}
	if reply.Applied {
		t.Fatal("reply.Applied = true, want false")
	}
	if reply.ConfigError == "" {
		t.Fatal("reply.ConfigError = empty, want decode error")
	}
}

func TestBridgeRepliesToHALDump(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridgeConn := b.NewConnection("fabric-bridge")
	go RunBridge(ctx, bridgeConn)
	time.Sleep(10 * time.Millisecond)

	writer := b.NewConnection("writer")
	req := b.NewConnection("requester")

	writer.Publish(writer.NewMessage(bus.T("hal", "state"), types.HALState{
		Level:  "ready",
		Status: "",
		TS:     123,
	}, true))

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer reqCancel()

	replyMsg, err := req.RequestWait(reqCtx, req.NewMessage(
		bus.T("rpc", "hal", "dump"),
		json.RawMessage(`{"ask":"status"}`),
		false,
	))
	if err != nil {
		t.Fatalf("RequestWait: %v", err)
	}

	reply, ok := replyMsg.Payload.(dumpReply)
	if !ok {
		t.Fatalf("reply payload type = %T, want dumpReply", replyMsg.Payload)
	}
	if !reply.OK {
		t.Fatalf("reply.OK = false: %+v", reply)
	}
	if reply.Method != "dump" {
		t.Fatalf("reply.Method = %q", reply.Method)
	}
	echo, ok := reply.Echo.(map[string]any)
	if !ok {
		t.Fatalf("reply.Echo type = %T, want map[string]any", reply.Echo)
	}
	if echo["ask"] != "status" {
		t.Fatalf("reply.Echo.ask = %#v", echo["ask"])
	}
	if reply.HAL == nil || reply.HAL.Level != "ready" || reply.HAL.TS != 123 {
		t.Fatalf("reply.HAL = %+v", reply.HAL)
	}
	if reply.Applied {
		t.Fatal("reply.Applied = true, want false")
	}
	if reply.ConfigCount != 0 {
		t.Fatalf("reply.ConfigCount = %d, want 0", reply.ConfigCount)
	}
}

func TestBridgeDumpReflectsAppliedConfig(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridgeConn := b.NewConnection("fabric-bridge")
	go RunBridge(ctx, bridgeConn)
	time.Sleep(10 * time.Millisecond)

	writer := b.NewConnection("writer")
	req := b.NewConnection("requester")

	in := types.HALConfig{
		Devices: []types.HALDevice{
			{ID: "led0", Type: "gpio_led"},
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	writer.Publish(writer.NewMessage(bus.T("config", "device"), json.RawMessage(raw), true))

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer reqCancel()

	replyMsg, err := req.RequestWait(reqCtx, req.NewMessage(
		bus.T("rpc", "hal", "dump"),
		json.RawMessage(`{"ask":"status"}`),
		false,
	))
	if err != nil {
		t.Fatalf("RequestWait: %v", err)
	}

	reply, ok := replyMsg.Payload.(dumpReply)
	if !ok {
		t.Fatalf("reply payload type = %T, want dumpReply", replyMsg.Payload)
	}
	if !reply.Applied {
		t.Fatal("reply.Applied = false, want true")
	}
	if reply.ConfigCount != 1 {
		t.Fatalf("reply.ConfigCount = %d, want 1", reply.ConfigCount)
	}
	if reply.ConfigError != "" {
		t.Fatalf("reply.ConfigError = %q, want empty", reply.ConfigError)
	}
}

func TestBridgeAcceptsLuaEmptyObjectsAsArrays(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridgeConn := b.NewConnection("fabric-bridge")
	go RunBridge(ctx, bridgeConn)
	time.Sleep(10 * time.Millisecond)

	writer := b.NewConnection("writer")
	reader := b.NewConnection("reader")
	sub := reader.Subscribe(bus.T("config", "hal"))
	defer reader.Unsubscribe(sub)

	// Lua encodes empty tables as {} (objects) not [] (arrays).
	writer.Publish(writer.NewMessage(bus.T("config", "device"),
		json.RawMessage(`{"devices":{},"pollers":{}}`), true))

	select {
	case msg := <-sub.Channel():
		if msg == nil {
			t.Fatal("config/hal subscription closed")
		}
		out, ok := msg.Payload.(types.HALConfig)
		if !ok {
			t.Fatalf("payload type = %T, want types.HALConfig", msg.Payload)
		}
		if out.Devices == nil || out.Pollers == nil {
			t.Fatalf("expected non-nil slices, got devices=%v pollers=%v", out.Devices, out.Pollers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for config/hal")
	}
}

func TestBridgeDumpDrainsPendingConfigBeforeReply(t *testing.T) {
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridgeConn := b.NewConnection("fabric-bridge")
	go RunBridge(ctx, bridgeConn)
	time.Sleep(10 * time.Millisecond)

	writer := b.NewConnection("writer")
	req := b.NewConnection("requester")

	in := types.HALConfig{
		Devices: []types.HALDevice{},
		Pollers: []types.PollSpec{},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	writer.Publish(writer.NewMessage(bus.T("config", "device"), json.RawMessage(raw), true))

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer reqCancel()

	replyMsg, err := req.RequestWait(reqCtx, req.NewMessage(
		bus.T("rpc", "hal", "dump"),
		json.RawMessage(`{"ask":"status"}`),
		false,
	))
	if err != nil {
		t.Fatalf("RequestWait: %v", err)
	}

	reply, ok := replyMsg.Payload.(dumpReply)
	if !ok {
		t.Fatalf("reply payload type = %T, want dumpReply", replyMsg.Payload)
	}
	if !reply.Applied {
		t.Fatal("reply.Applied = false, want true")
	}
	if reply.ConfigCount != 1 {
		t.Fatalf("reply.ConfigCount = %d, want 1", reply.ConfigCount)
	}
}

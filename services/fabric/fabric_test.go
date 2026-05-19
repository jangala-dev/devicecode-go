package fabric

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/types"
	"devicecode-go/x/shmring"
)

func pipePair() (*rwTransport, *rwTransport) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	return newRWTransport(r2, w1), newRWTransport(r1, w2)
}

func newBus() *bus.Bus { return bus.NewBus(3, "+", "#") }

type captureTransport struct {
	writes   [][]byte
	writeErr error
}

func (t *captureTransport) ReadLine() ([]byte, error) { return nil, io.EOF }

func (t *captureTransport) WriteLine(data []byte) error {
	if t.writeErr != nil {
		return t.writeErr
	}
	cp := append([]byte(nil), data...)
	t.writes = append(t.writes, cp)
	return nil
}

func (t *captureTransport) Close() error { return nil }

func readMsg[T any](t *testing.T, tr Transport) T {
	t.Helper()
	line, err := tr.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	var msg T
	if err := json.Unmarshal(line, &msg); err != nil {
		t.Fatalf("Unmarshal %q: %v", line, err)
	}
	return msg
}

func sendMsg(t *testing.T, tr Transport, v any) {
	t.Helper()
	b := marshal(v)
	if err := tr.WriteLine(b[:len(b)-1]); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
}

const testCM5SID = "s1"

func bringUp(t *testing.T, cm5 Transport) protoHelloAck {
	t.Helper()
	sendMsg(t, cm5, protoHello{
		Type: "hello", Node: "cm5-local", Peer: "mcu-1", SID: testCM5SID, Proto: protoVersion,
	})
	ack := readMsg[protoHelloAck](t, cm5)
	if !ack.OK || ack.Node != "mcu-1" || ack.SID == "" || ack.Proto != protoVersion {
		t.Fatalf("bad hello_ack: %+v", ack)
	}
	time.Sleep(50 * time.Millisecond)
	return ack
}

func unlockExports(t *testing.T, cm5 Transport) {
	t.Helper()
	sendMsg(t, cm5, protoPing{Type: "ping", TS: 77, SID: testCM5SID})
	pong := readMsg[protoPong](t, cm5)
	if pong.Type != "pong" {
		t.Fatalf("expected pong, got %q", pong.Type)
	}
}

// ---- codec ----

func TestCodecRoundTrip(t *testing.T) {
	orig := protoHello{Type: "hello", Node: "mcu-1", Peer: "cm5-local", SID: "abc", Proto: protoVersion}
	data := marshal(orig)
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Error("marshal should end with newline")
	}
	jsonPart := data[:len(data)-1]
	if bytes.Contains(jsonPart, []byte("\n")) {
		t.Error("JSON should not contain embedded newlines")
	}
	if protoType(jsonPart) != "hello" {
		t.Errorf("protoType = %q", protoType(jsonPart))
	}
	var dec protoHello
	json.Unmarshal(jsonPart, &dec)
	if dec != orig {
		t.Errorf("round-trip: %+v vs %+v", dec, orig)
	}
}

func TestCodecAllTypes(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want string
	}{
		{protoHello{Type: "hello"}, "hello"},
		{protoHelloAck{Type: "hello_ack"}, "hello_ack"},
		{protoPing{Type: "ping", TS: 1}, "ping"},
		{protoPong{Type: "pong", TS: 2}, "pong"},
		{protoPub{Type: "pub", Topic: []string{"a"}}, "pub"},
		{protoUnretain{Type: "unretain", Topic: []string{"a"}}, "unretain"},
		{protoCall{Type: "call", ID: "c1"}, "call"},
		{protoReply{Type: "reply", Corr: "c1", OK: true}, "reply"},
		{protoXferBegin{Type: "xfer_begin", XferID: "x1"}, "xfer_begin"},
		{protoXferReady{Type: "xfer_ready", XferID: "x1"}, "xfer_ready"},
		{protoXferChunk{Type: "xfer_chunk", XferID: "x1"}, "xfer_chunk"},
		{protoXferNeed{Type: "xfer_need", XferID: "x1"}, "xfer_need"},
		{protoXferCommit{Type: "xfer_commit", XferID: "x1"}, "xfer_commit"},
		{protoXferDone{Type: "xfer_done", XferID: "x1"}, "xfer_done"},
		{protoXferAbort{Type: "xfer_abort", XferID: "x1", Err: "aborted"}, "xfer_abort"},
	} {
		b := marshal(tc.v)
		if got := protoType(b[:len(b)-1]); got != tc.want {
			t.Errorf("protoType = %q, want %q", got, tc.want)
		}
	}
}

func TestWireTypeBadInput(t *testing.T) {
	for _, b := range [][]byte{[]byte("not json"), []byte(`{"no_type":true}`), nil} {
		if got := protoType(b); got != "" {
			t.Errorf("protoType(%q) = %q, want empty", b, got)
		}
	}
}

func TestWireTypeIgnoresNestedTypeKeys(t *testing.T) {
	// protoType must return the top-level discriminator, not a nested
	// payload.type / meta.type key. The previous heuristic-only scan
	// would mis-route e.g. a `pub` with a payload that happened to
	// contain its own "type" field.
	for _, tc := range []struct {
		line []byte
		want string
	}{
		// Nested payload object with its own "type":
		{[]byte(`{"payload":{"type":"x"},"type":"pub"}`), "pub"},
		// Nested type appears before the real top-level type:
		{[]byte(`{"meta":{"type":"firmware"},"type":"xfer_begin","xfer_id":"a"}`), "xfer_begin"},
		// Type buried inside an array element:
		{[]byte(`{"topic":["a","type","b"],"type":"unretain"}`), "unretain"},
		// Type as a substring of a value (must NOT match):
		{[]byte(`{"id":"my-type-here","type":"call"}`), "call"},
		// Real-world hello shape from CM5 (regression for the malformed-frame bug):
		{[]byte(`{"sid":"a08590c4-afb8-4a23-ae39-ded871a3d433","node":"cm5","type":"hello"}`), "hello"},
	} {
		if got := protoType(tc.line); got != tc.want {
			t.Errorf("protoType(%s) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// ---- transport ----

func TestTransportRoundTrip(t *testing.T) {
	a, b := pipePair()
	done := make(chan struct{})
	go func() {
		defer close(done)
		line, err := b.ReadLine()
		if err != nil {
			t.Errorf("ReadLine: %v", err)
			return
		}
		if string(line) != `{"type":"ping","ts":99}` {
			t.Errorf("got %q", line)
		}
	}()
	sendMsg(t, a, protoPing{Type: "ping", TS: 99})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestOversizeLineRecovery(t *testing.T) {
	big := `{"type":"ping","ts":0,"x":"` + strings.Repeat("x", maxLineLen+100) + `"}`
	input := big + "\n" + `{"type":"ping","ts":3}` + "\n"
	tr := newRWTransport(strings.NewReader(input), io.Discard)
	_, err := tr.ReadLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
	line, err := tr.ReadLine()
	if err != nil {
		t.Fatalf("second ReadLine: %v", err)
	}
	if string(line) != `{"type":"ping","ts":3}` {
		t.Errorf("got %q", line)
	}
}

// ---- shmring transport ----

func TestShmringTransportRoundTrip(t *testing.T) {
	rx := shmring.New(256)
	tx := shmring.New(256)
	mcuTr := NewShmringTransport(rx, tx)
	defer mcuTr.Close()

	rx.TryWriteFrom([]byte(`{"type":"ping","ts":42}` + "\n"))
	line, err := mcuTr.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if string(line) != `{"type":"ping","ts":42}` {
		t.Errorf("got %q", line)
	}

	if err := mcuTr.WriteLine([]byte(`{"type":"pong","ts":42}`)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	var out [128]byte
	n := tx.TryReadInto(out[:])
	if string(out[:n]) != `{"type":"pong","ts":42}`+"\n" {
		t.Errorf("tx got %q", out[:n])
	}
}

func TestShmringTransportMultiLine(t *testing.T) {
	rx := shmring.New(256)
	tr := NewShmringTransport(rx, shmring.New(256))
	defer tr.Close()
	rx.TryWriteFrom([]byte(`{"type":"ping","ts":1}` + "\n" + `{"type":"ping","ts":2}` + "\n"))
	line1, _ := tr.ReadLine()
	line2, _ := tr.ReadLine()
	if string(line1) != `{"type":"ping","ts":1}` {
		t.Errorf("line1 = %q", line1)
	}
	if string(line2) != `{"type":"ping","ts":2}` {
		t.Errorf("line2 = %q", line2)
	}
}

func TestShmringTransportReadLineWrapsAcrossSegments(t *testing.T) {
	rx := shmring.New(8)
	tr := NewShmringTransport(rx, shmring.New(8))
	defer tr.Close()

	rx.TryWriteFrom([]byte("123456"))
	var discard [6]byte
	if n := rx.TryReadInto(discard[:]); n != len(discard) {
		t.Fatalf("priming read = %d, want %d", n, len(discard))
	}
	if n := rx.TryWriteFrom([]byte("ab\n")); n != 3 {
		t.Fatalf("wrapped write = %d, want 3", n)
	}

	line, err := tr.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if string(line) != "ab" {
		t.Errorf("got %q", line)
	}
}

func TestShmringTransportWriteLineWrapsAcrossSegments(t *testing.T) {
	tx := shmring.New(8)
	tr := NewShmringTransport(shmring.New(8), tx)
	defer tr.Close()

	tx.TryWriteFrom([]byte("123456"))
	var discard [6]byte
	if n := tx.TryReadInto(discard[:]); n != len(discard) {
		t.Fatalf("priming read = %d, want %d", n, len(discard))
	}

	if err := tr.WriteLine([]byte("ab")); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	var out [3]byte
	if n := tx.TryReadInto(out[:]); n != len(out) {
		t.Fatalf("wrapped read = %d, want %d", n, len(out))
	}
	if string(out[:]) != "ab\n" {
		t.Errorf("tx got %q", out[:])
	}
}

func TestShmringTransportOversize(t *testing.T) {
	// Ring must be larger than maxLineLen+100 + newline + the trailing ping
	// frame so the producer can deposit both lines without blocking. The rx
	// ring used to be 4096 when maxLineLen=2048, leaving comfortable
	// headroom; now that maxLineLen=4096, bump to 8192.
	rx := shmring.New(8192)
	tr := NewShmringTransport(rx, shmring.New(256))
	defer tr.Close()
	big := make([]byte, maxLineLen+100)
	for i := range big {
		big[i] = 'x'
	}
	rx.TryWriteFrom(big)
	rx.TryWriteFrom([]byte("\n"))
	rx.TryWriteFrom([]byte(`{"type":"ping","ts":7}` + "\n"))
	_, err := tr.ReadLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
	line, err := tr.ReadLine()
	if err != nil {
		t.Fatalf("second ReadLine: %v", err)
	}
	if string(line) != `{"type":"ping","ts":7}` {
		t.Errorf("got %q", line)
	}
}

func TestShmringTransportCloseUnblocks(t *testing.T) {
	tr := NewShmringTransport(shmring.New(256), shmring.New(256))
	done := make(chan struct{})
	go func() { tr.ReadLine(); close(done) }()
	tr.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadLine did not unblock")
	}
}

// ---- handshake ----

func TestHandshake(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())

	sendMsg(t, cm5, protoHello{
		Type: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s1", Proto: protoVersion,
	})
	ack := readMsg[protoHelloAck](t, cm5)
	if !ack.OK || ack.Node != "mcu-1" || ack.SID == "" || ack.Proto != protoVersion {
		t.Errorf("bad ack: %+v", ack)
	}
	time.Sleep(50 * time.Millisecond)
	sendMsg(t, cm5, protoPing{Type: "ping", TS: 99, SID: "s1"})
	pong := readMsg[protoPong](t, cm5)
	if pong.TS != 99 || pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestSessionReset(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	sendMsg(t, cm5, protoHello{Type: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s2", Proto: protoVersion})
	ack := readMsg[protoHelloAck](t, cm5)
	if !ack.OK || ack.SID == "" || ack.Proto != protoVersion {
		t.Error("hello_ack.OK = false")
	}
	sendMsg(t, cm5, protoPing{Type: "ping", TS: 55, SID: "s2"})
	pong := readMsg[protoPong](t, cm5)
	if pong.TS != 55 || pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestRejectsWrongPeer(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())

	sendMsg(t, cm5, protoHello{Type: "hello", Node: "cm5-local", Peer: "mcu-999", SID: "s1", Proto: protoVersion})
	gotLine := make(chan readResult, 1)
	go func() {
		line, err := cm5.ReadLine()
		gotLine <- readResult{line: line, err: err}
	}()
	select {
	case <-gotLine:
		t.Fatal("got response to wrong-peer hello")
	case <-time.After(200 * time.Millisecond):
	}
	sendMsg(t, cm5, protoHello{Type: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s2", Proto: protoVersion})
	select {
	case res := <-gotLine:
		if res.err != nil {
			t.Fatalf("ReadLine error: %v", res.err)
		}
		var ack protoHelloAck
		if err := json.Unmarshal(res.line, &ack); err != nil {
			t.Fatalf("expected hello_ack: %v", err)
		}
		if !ack.OK {
			t.Fatal("hello_ack.OK = false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no hello_ack for correct peer")
	}
}

func TestRejectsMissingNodeWhenPeerPinned(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())

	gotLine := make(chan readResult, 1)
	go func() {
		line, err := cm5.ReadLine()
		gotLine <- readResult{line: line, err: err}
	}()

	sendMsg(t, cm5, protoHello{Type: "hello", Peer: "mcu-1", SID: "s1", Proto: protoVersion})
	select {
	case <-gotLine:
		t.Fatal("got response to hello without node")
	case <-time.After(200 * time.Millisecond):
	}

	sendMsg(t, cm5, protoHello{Type: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s2", Proto: protoVersion})
	select {
	case res := <-gotLine:
		if res.err != nil {
			t.Fatalf("ReadLine error: %v", res.err)
		}
		var ack protoHelloAck
		if err := json.Unmarshal(res.line, &ack); err != nil {
			t.Fatalf("expected hello_ack: %v", err)
		}
		if !ack.OK {
			t.Fatal("hello_ack.OK = false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no hello_ack for correct peer")
	}
}

func TestPingPong(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	ack := bringUp(t, cm5)
	sendMsg(t, cm5, protoPing{Type: "ping", TS: 42, SID: "s1"})
	pong := readMsg[protoPong](t, cm5)
	if pong.TS != 42 || pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestMCUNeverInitiates(t *testing.T) {
	// Pre-handshake the MCU is silent; tickPing only fires once the link
	// is up. Active outbound pings post-handshake are covered by
	// TestSessionPingsUnconditionally.
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	gotLine := make(chan struct{})
	go func() { cm5.ReadLine(); close(gotLine) }()
	select {
	case <-gotLine:
		t.Fatal("MCU sent unsolicited message")
	case <-time.After(2 * time.Second):
	}
	cancel()
}

func TestSessionPingsUnconditionally(t *testing.T) {
	// session_ctl.lua resets next_ping_at = now + ping_interval after
	// every send, with no TX-activity dependency. Once the link is up,
	// pings must keep flowing even if neither side talks otherwise.
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", LinkConfig{PingInterval: 150 * time.Millisecond})
	bringUp(t, cm5)

	for i := 0; i < 3; i++ {
		ping := readMsg[protoPing](t, cm5)
		if ping.Type != msgPing {
			t.Fatalf("ping[%d] type = %q, want %q", i, ping.Type, msgPing)
		}
	}
}

func TestReadyHeldUntilExportHoldoff(t *testing.T) {
	// session_ctl.lua / rpc_bridge.lua: ready == established and rpc_ready,
	// where rpc_ready edges true only after retained replay completes.
	// The Go side gates rpcReady on exportReadyAt elapsing post-handshake.
	mcu, cm5 := pipePair()
	b := newBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(bus.T("state", "fabric", "link", "mcu0"))
	defer observer.Unsubscribe(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	var sawNotReady, sawReady bool
	deadline := time.After(3 * time.Second)
	for !sawReady {
		select {
		case msg := <-sub.Channel():
			payload, ok := msg.Payload.(linkStatePayload)
			if !ok {
				t.Fatalf("payload type = %T", msg.Payload)
			}
			if payload.Established && !payload.Ready {
				sawNotReady = true
			}
			if payload.Ready {
				if !sawNotReady {
					t.Fatalf("Ready edge raised without prior Established+!Ready state")
				}
				sawReady = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for Ready=true")
		}
	}
}

func TestSessionResetUnretainsImports(t *testing.T) {
	// rpc_bridge.lua's invalidate_imported_retained clears every imported
	// retained slot on session-generation bump. The Go side mirrors this
	// in promoteLink/teardownImportedRetained: each tracked local topic
	// gets a nil-payload retained publish that clears the bus's retain
	// store, so consumers don't see stale CM5-session data.
	mcu, cm5 := pipePair()
	b := newBus()
	observer := b.NewConnection("observer")
	cfgSub := observer.Subscribe(tConfigHAL)
	defer observer.Unsubscribe(cfgSub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	// Push a config via the import pub path so config/hal becomes a
	// tracked imported retain.
	sendMsg(t, cm5, protoPub{
		Type:    msgPub,
		Topic:   []string{"config", "device"},
		Payload: json.RawMessage(`{"devices":[]}`),
		Retain:  true,
	})

	// Observe the local retain (non-nil payload).
	deadline := time.After(2 * time.Second)
	gotInitial := false
	for !gotInitial {
		select {
		case msg := <-cfgSub.Channel():
			if msg.Retained && msg.Payload != nil {
				gotInitial = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for initial config/hal retain")
		}
	}

	// Force a session reset: hello with a new SID. Concurrent reader
	// drains the new hello_ack the MCU sends back; pipePair is
	// synchronous so without this the MCU's sendControl would block,
	// promoteLink would never fire, and teardownImportedRetained would
	// not run.
	go func() { _ = readMsg[protoHelloAck](t, cm5) }()
	sendMsg(t, cm5, protoHello{
		Type: msgHello,
		Node: "cm5-local",
		Peer: "mcu-1",
		SID:  "cm5-sid-new",
	})

	// Expect a nil-payload retained publish on config/hal.
	deadline = time.After(2 * time.Second)
	for {
		select {
		case msg := <-cfgSub.Channel():
			if msg.Retained && msg.Payload == nil {
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for unretain after session reset")
		}
	}
}

func TestSessionResetUnretainsImportsAfterTransientPub(t *testing.T) {
	// Regression: a non-retained pub arriving on the same imported topic
	// after an earlier retained pub must NOT untrack — the bus retain
	// store still holds the prior retained value (the bus only clears it
	// on explicit unretain/retained-nil). Without this, the stale retain
	// would survive a session-reset because we'd think nothing was tracked.
	prev := importPublishRules
	importPublishRules = append([]importRule{}, prev...)
	importPublishRules = append(importPublishRules, importRule{
		wire:  []string{"telem", "device", "fast"},
		local: []string{"telem", "hal", "fast"},
	})
	t.Cleanup(func() { importPublishRules = prev })

	mcu, cm5 := pipePair()
	b := newBus()
	observer := b.NewConnection("observer")
	subFast := observer.Subscribe(bus.T("telem", "hal", "fast"))
	defer observer.Unsubscribe(subFast)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	// 1) Retained import — establishes the bus retain + tracking entry.
	sendMsg(t, cm5, protoPub{
		Type:    msgPub,
		Topic:   []string{"telem", "device", "fast"},
		Payload: json.RawMessage(`{"v":1}`),
		Retain:  true,
	})

	// Drain until we see the retained payload.
	deadline := time.After(2 * time.Second)
	gotRetain := false
	for !gotRetain {
		select {
		case msg := <-subFast.Channel():
			if msg.Retained && msg.Payload != nil {
				gotRetain = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for initial retained pub")
		}
	}

	// 2) Non-retained pub on same topic — must not untrack.
	sendMsg(t, cm5, protoPub{
		Type:    msgPub,
		Topic:   []string{"telem", "device", "fast"},
		Payload: json.RawMessage(`{"v":2}`),
		Retain:  false,
	})
	// Best-effort drain so the next subFast read sees the unretain edge.
	deadline = time.After(500 * time.Millisecond)
	draining := true
	for draining {
		select {
		case <-subFast.Channel():
		case <-deadline:
			draining = false
		}
	}

	// 3) Session reset → expect the original retain to be cleared.
	go func() { _ = readMsg[protoHelloAck](t, cm5) }()
	sendMsg(t, cm5, protoHello{
		Type: msgHello,
		Node: "cm5-local",
		Peer: "mcu-1",
		SID:  "cm5-sid-new",
	})

	deadline = time.After(2 * time.Second)
	for {
		select {
		case msg := <-subFast.Channel():
			if msg.Retained && msg.Payload == nil {
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for unretain after session reset")
		}
	}
}

func TestWriterControlPreemptsRPCAndBulk(t *testing.T) {
	// writer.lua drains the control lane first (no fairness); then
	// weighted RR between rpc and bulk. Pre-load all three lanes and
	// assert the drain order is: all control, then 4 rpc, then 1 bulk,
	// then any remaining rpc/bulk (default rpc_quantum=4, bulk_quantum=1).
	tr := &captureTransport{}
	s := session{tr: tr, cfg: DefaultLinkConfig()}
	s.txBulk.push([]byte(`{"type":"xfer_chunk","i":0}`))
	s.txBulk.push([]byte(`{"type":"xfer_chunk","i":1}`))
	for i := 0; i < 5; i++ {
		s.txRPC.push([]byte(`{"type":"pub","i":` + string(rune('0'+i)) + `}`))
	}
	s.txControl.push([]byte(`{"type":"ping"}`))
	s.txControl.push([]byte(`{"type":"xfer_need"}`))

	if !s.flushWriter() {
		t.Fatal("flushWriter returned false")
	}
	if len(tr.writes) != 9 {
		t.Fatalf("writes = %d, want 9", len(tr.writes))
	}
	// Control drains first.
	want := []string{
		`{"type":"ping"}`,
		`{"type":"xfer_need"}`,
		// Then RR: 4 rpc, 1 bulk, 1 rpc, 1 bulk, 0 (no more bulk; remaining rpc).
		`{"type":"pub","i":0}`,
		`{"type":"pub","i":1}`,
		`{"type":"pub","i":2}`,
		`{"type":"pub","i":3}`,
		`{"type":"xfer_chunk","i":0}`,
		`{"type":"pub","i":4}`,
		`{"type":"xfer_chunk","i":1}`,
	}
	for i, w := range want {
		if string(tr.writes[i]) != w {
			t.Fatalf("write[%d] = %q, want %q", i, tr.writes[i], w)
		}
	}
}

func TestInboundCallBusyAtCapacity(t *testing.T) {
	// rpc_bridge.lua's spawn_local_call_helper rejects with err="busy"
	// when inbound_helpers >= max_inbound_helpers, before the route check.
	// With MaxInboundHelpers=1, the second concurrent inbound call must
	// reply busy without going through routing.
	prev := importCallRules
	importCallRules = append([]importRule{}, prev...)
	importCallRules = append(importCallRules, importRule{
		wire:  []string{"rpc", "test", "noop"},
		local: []string{"rpc", "test", "noop"},
	})
	t.Cleanup(func() { importCallRules = prev })

	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", LinkConfig{MaxInboundHelpers: 1})
	bringUp(t, cm5)

	// First call holds the only helper slot. The bus has no handler, so
	// the call sits as a pending request until timeout.
	sendMsg(t, cm5, protoCall{
		Type:      msgCall,
		ID:        "c1",
		Topic:     []string{"rpc", "test", "noop"},
		Payload:   json.RawMessage(`{}`),
		TimeoutMs: 5000,
	})

	// Second call arrives while the helper is full → busy reply.
	sendMsg(t, cm5, protoCall{
		Type:    msgCall,
		ID:      "c2",
		Topic:   []string{"rpc", "test", "noop"},
		Payload: json.RawMessage(`{}`),
	})

	reply := readMsg[protoReply](t, cm5)
	if reply.Corr != "c2" {
		t.Fatalf("first reply corr = %q, want c2", reply.Corr)
	}
	if reply.OK || reply.Err != "busy" {
		t.Fatalf("expected busy reply for c2, got %+v", reply)
	}
}

func TestUnknownTypeIgnored(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)
	cm5.WriteLine([]byte(`{"type":"future_msg"}`))
	sendMsg(t, cm5, protoPing{Type: "ping", TS: 1})
	pong := readMsg[protoPong](t, cm5)
	if pong.TS != 1 {
		t.Errorf("pong.TS = %d", pong.TS)
	}
}

func TestMalformedJSONIgnored(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)
	cm5.WriteLine([]byte("not json"))
	sendMsg(t, cm5, protoPing{Type: "ping", TS: 2})
	pong := readMsg[protoPong](t, cm5)
	if pong.TS != 2 {
		t.Errorf("pong.TS = %d", pong.TS)
	}
}

func TestCancelClosesCleanly(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
		close(done)
	}()
	bringUp(t, cm5)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestLinkStatePublishedOnHandshake(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(bus.T("state", "fabric", "link", "mcu0"))
	defer observer.Unsubscribe(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())

	ack := bringUp(t, cm5)

	var sawOpening bool
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sub.Channel():
			if msg == nil {
				t.Fatal("nil link-state message")
			}
			payload, ok := msg.Payload.(linkStatePayload)
			if !ok {
				t.Fatalf("payload type = %T, want linkStatePayload", msg.Payload)
			}
			if payload.Status == "opening" {
				sawOpening = true
			}
			if payload.Status == "ready" {
				if payload.LinkID != "mcu0" {
					t.Fatalf("link_id = %q, want mcu0", payload.LinkID)
				}
				if !payload.Ready || !payload.Established {
					t.Fatalf("expected ready/established link state, got %+v", payload)
				}
				if payload.PeerID != "cm5-local" {
					t.Fatalf("peer_id = %q, want cm5-local", payload.PeerID)
				}
				if payload.LocalSID != ack.SID {
					t.Fatalf("local_sid = %q, want %q", payload.LocalSID, ack.SID)
				}
				if payload.PeerSID != "s1" {
					t.Fatalf("peer_sid = %q, want s1", payload.PeerSID)
				}
				if !sawOpening {
					t.Fatal("did not observe opening link state before ready")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for ready link state")
		}
	}
}

// ---- remap ----

func topicString(t bus.Topic) string {
	if t == nil {
		return ""
	}
	var parts []string
	for i := 0; i < t.Len(); i++ {
		parts = append(parts, t.At(i).(string))
	}
	return strings.Join(parts, "/")
}

func TestImportPublishTopic(t *testing.T) {
	for _, tc := range []struct {
		wire []string
		want string
	}{
		{[]string{"config", "device"}, "config/hal"},
		{[]string{"config", "other"}, ""},
		{[]string{"unknown", "x"}, ""},
		{nil, ""},
	} {
		got := importPublishTopic(tc.wire)
		if gotStr := topicString(got); gotStr != tc.want {
			t.Errorf("importPublishTopic(%v) = %q, want %q", tc.wire, gotStr, tc.want)
		}
	}
}

func TestImportCallTopic(t *testing.T) {
	for _, tc := range []struct {
		wire []string
		want string
	}{
		// rpc/hal/dump is handled directly by onCall, not via import rules.
		{[]string{"rpc", "hal", "other"}, ""},
		{[]string{"config", "device"}, ""},
		{nil, ""},
	} {
		got := importCallTopic(tc.wire)
		if gotStr := topicString(got); gotStr != tc.want {
			t.Errorf("importCallTopic(%v) = %q, want %q", tc.wire, gotStr, tc.want)
		}
	}
}

func TestExportTopic(t *testing.T) {
	for _, tc := range []struct {
		bus  bus.Topic
		want []string
	}{
		{bus.T("hal", "cap", "env", "temperature", "core", "value"), []string{"state", "env", "temperature", "core", "value"}},
		{bus.T("hal", "cap", "power", "battery", "internal", "value"), []string{"state", "power", "battery", "internal", "value"}},
		{bus.T("hal", "state"), []string{"state", "hal"}},
		{bus.T("hal", "cap", "gpio", "fan", "value"), nil},
		{bus.T("other", "topic"), nil},
	} {
		got := exportTopic(tc.bus)
		if tc.want == nil {
			if got != nil {
				t.Errorf("exportTopic(%v) = %v, want nil", tc.bus, got)
			}
		} else {
			if !slicesEqual(got, tc.want) {
				t.Errorf("exportTopic(%v) = %v, want %v", tc.bus, got, tc.want)
			}
		}
	}
}

func TestExportCallTopic(t *testing.T) {
	for _, tc := range []struct {
		bus  bus.Topic
		want []string
	}{
		{bus.T("fabric", "out", "rpc", "hal", "dump"), []string{"rpc", "hal", "dump"}},
		{bus.T("fabric", "out", "rpc", "hal"), nil},
		{bus.T("other", "topic"), nil},
	} {
		got := exportCallTopic(tc.bus)
		if tc.want == nil {
			if got != nil {
				t.Errorf("exportCallTopic(%v) = %v, want nil", tc.bus, got)
			}
		} else if !slicesEqual(got, tc.want) {
			t.Errorf("exportCallTopic(%v) = %v, want %v", tc.bus, got, tc.want)
		}
	}
}

func TestExportCallPatterns(t *testing.T) {
	patterns := exportCallPatterns()
	if len(patterns) != 1 {
		t.Fatalf("len(exportCallPatterns()) = %d, want 1", len(patterns))
	}
	if got := topicString(patterns[0]); got != "fabric/out/rpc/hal/dump" {
		t.Fatalf("exportCallPatterns()[0] = %q, want fabric/out/rpc/hal/dump", got)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- pub import ----

func TestPubImport(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	conn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, conn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	reader := b.NewConnection("test")
	sub := reader.Subscribe(bus.T("config", "hal"))

	sendMsg(t, cm5, protoPub{
		Type:    "pub",
		Topic:   []string{"config", "device"},
		Payload: json.RawMessage(`{"devices":[],"pollers":[]}`),
		Retain:  true,
	})

	select {
	case m := <-sub.Channel():
		if m == nil {
			t.Fatal("nil message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for imported config on config/hal")
	}
}

// ---- pub export ----

func TestPubExport(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	publishConn := b.NewConnection("hal")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)
	unlockExports(t, cm5)

	publishConn.Publish(publishConn.NewMessage(
		bus.T("hal", "cap", "env", "temperature", "core", "value"),
		map[string]int{"deci_c": 412},
		true,
	))

	msg := readMsg[protoPub](t, cm5)
	if msg.Type != "pub" {
		t.Fatalf("expected pub, got %q", msg.Type)
	}
	want := []string{"state", "env", "temperature", "core", "value"}
	if !slicesEqual(msg.Topic, want) {
		t.Errorf("topic = %v, want %v", msg.Topic, want)
	}
	if !msg.Retain {
		t.Error("expected retain=true")
	}
}

func TestUnretainExport(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	publishConn := b.NewConnection("hal")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)
	unlockExports(t, cm5)

	// Publish retained value first.
	publishConn.Publish(publishConn.NewMessage(
		bus.T("hal", "cap", "env", "temperature", "core", "value"),
		map[string]int{"deci_c": 412},
		true,
	))
	pub := readMsg[protoPub](t, cm5)
	if pub.Type != "pub" || !pub.Retain {
		t.Fatalf("expected retained pub, got t=%q retain=%v", pub.Type, pub.Retain)
	}

	// Clear retained state (retain=true, payload=nil).
	publishConn.Publish(publishConn.NewMessage(
		bus.T("hal", "cap", "env", "temperature", "core", "value"),
		nil,
		true,
	))
	unr := readMsg[protoUnretain](t, cm5)
	if unr.Type != "unretain" {
		t.Fatalf("expected unretain, got %q", unr.Type)
	}
	want := []string{"state", "env", "temperature", "core", "value"}
	if !slicesEqual(unr.Topic, want) {
		t.Errorf("topic = %v, want %v", unr.Topic, want)
	}
}

func TestDrainExportsReturnsWhenSubscriptionClosed(t *testing.T) {
	b := newBus()
	conn := b.NewConnection("fabric")
	sub := conn.Subscribe(bus.T("state", "#"))
	conn.Unsubscribe(sub)

	s := session{
		link:       linkUp,
		exportSubs: []*bus.Subscription{sub},
	}

	done := make(chan struct{})
	go func() {
		s.drainExports()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainExports did not return")
	}
}

func TestDrainExportsWaitsForStartupHoldoff(t *testing.T) {
	b := newBus()
	conn := b.NewConnection("fabric")
	pub := b.NewConnection("hal")
	sub := conn.Subscribe(bus.T("hal", "cap", "env", "#"))
	defer conn.Unsubscribe(sub)

	msg := pub.NewMessage(
		bus.T("hal", "cap", "env", "temperature", "core", "value"),
		map[string]int{"deci_c": 412},
		true,
	)

	s := session{
		link:           linkUp,
		exportsEnabled: true,
		exportSubs:     []*bus.Subscription{sub},
		exportReadyAt:  time.Now().Add(time.Second),
	}

	pub.Publish(msg)

	done := make(chan struct{})
	go func() {
		s.drainExports()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainExports did not return")
	}
}

// ---- unretain ----

func TestPubIgnoredBeforeHandshake(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())

	sendMsg(t, cm5, protoPub{
		Type: "pub", Topic: []string{"config", "device"},
		Payload: json.RawMessage(`{"v":1}`), Retain: true,
	})
	time.Sleep(50 * time.Millisecond)

	reader := b.NewConnection("test")
	sub := reader.Subscribe(bus.T("config", "device"))
	defer reader.Unsubscribe(sub)
	select {
	case m := <-sub.Channel():
		t.Fatalf("unexpected pre-handshake publish: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUnretainIgnoredBeforeHandshake(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())

	writer := b.NewConnection("writer")
	writer.Publish(writer.NewMessage(bus.T("config", "device"), json.RawMessage(`{"v":1}`), true))

	reader := b.NewConnection("test")
	sub := reader.Subscribe(bus.T("config", "device"))
	defer reader.Unsubscribe(sub)
	select {
	case m := <-sub.Channel():
		if m == nil || m.Payload == nil {
			t.Fatalf("expected retained config/device, got %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for retained config/device")
	}

	sendMsg(t, cm5, protoUnretain{Type: "unretain", Topic: []string{"config", "device"}})
	select {
	case m := <-sub.Channel():
		t.Fatalf("unexpected pre-handshake unretain effect: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUnretain(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	conn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, conn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	sendMsg(t, cm5, protoPub{
		Type: "pub", Topic: []string{"config", "device"},
		Payload: json.RawMessage(`{"v":1}`), Retain: true,
	})
	time.Sleep(50 * time.Millisecond)
	sendMsg(t, cm5, protoUnretain{Type: "unretain", Topic: []string{"config", "device"}})
	time.Sleep(50 * time.Millisecond)

	reader := b.NewConnection("test")
	sub := reader.Subscribe(bus.T("config", "device"))
	select {
	case m := <-sub.Channel():
		if m != nil && m.Payload != nil {
			t.Errorf("expected no retained message, got %+v", m)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

// ---- call import ----

func TestCallIgnoredBeforeHandshake(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())

	handler := b.NewConnection("handler")
	sub := handler.Subscribe(bus.T("rpc", "hal", "dump"))
	defer handler.Unsubscribe(sub)

	sendMsg(t, cm5, protoCall{
		Type: "call", ID: "pre-hello-1", Topic: []string{"rpc", "hal", "dump"},
		Payload: json.RawMessage(`{}`), TimeoutMs: 5000,
	})

	select {
	case m := <-sub.Channel():
		t.Fatalf("unexpected pre-handshake call dispatch: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCallImport(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	handler := b.NewConnection("handler")
	sub := handler.Subscribe(bus.T("rpc", "hal", "dump"))
	go func() {
		for m := range sub.Channel() {
			handler.Reply(m, map[string]string{"result": "ok"}, false)
		}
	}()

	sendMsg(t, cm5, protoCall{
		Type: "call", ID: "test-corr-1", Topic: []string{"rpc", "hal", "dump"},
		Payload: json.RawMessage(`{}`), TimeoutMs: 5000,
	})

	reply := readMsg[protoReply](t, cm5)
	if reply.Corr != "test-corr-1" {
		t.Errorf("corr = %q", reply.Corr)
	}
	if !reply.OK {
		t.Errorf("reply not ok: %s", reply.Err)
	}
}

func TestCallNoRoute(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	sendMsg(t, cm5, protoCall{
		Type: "call", ID: "no-route-1", Topic: []string{"unknown", "endpoint"},
		Payload: json.RawMessage(`{}`), TimeoutMs: 1000,
	})

	reply := readMsg[protoReply](t, cm5)
	if reply.Corr != "no-route-1" {
		t.Errorf("corr = %q", reply.Corr)
	}
	if reply.OK {
		t.Error("expected ok=false")
	}
	if reply.Err != "no_route" {
		t.Errorf("err = %q, want no_route", reply.Err)
	}
}

func TestDumpCallReturnsConfigState(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	// Send config first so the session has state.
	sendMsg(t, cm5, protoPub{
		Type:    "pub",
		Topic:   []string{"config", "device"},
		Payload: json.RawMessage(`{"devices":[],"pollers":[]}`),
		Retain:  true,
	})
	time.Sleep(100 * time.Millisecond)

	// Call dump.
	sendMsg(t, cm5, protoCall{
		Type: "call", ID: "dump-1", Topic: []string{"rpc", "hal", "dump"},
		Payload: json.RawMessage(`{"ask":"status"}`), TimeoutMs: 5000,
	})

	reply := readMsg[protoReply](t, cm5)
	if reply.Corr != "dump-1" {
		t.Errorf("corr = %q", reply.Corr)
	}
	if !reply.OK {
		t.Errorf("expected ok=true, got err=%q", reply.Err)
	}
	var dump dumpReply
	if err := json.Unmarshal(reply.Value, &dump); err != nil {
		t.Fatalf("unmarshal dump reply: %v", err)
	}
	if !dump.Applied {
		t.Error("expected applied=true")
	}
	if dump.ConfigCount != 1 {
		t.Errorf("config_count = %d, want 1", dump.ConfigCount)
	}
}

func TestDumpCallDoesNotBlockPing(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)

	// Send dump call and ping back-to-back.
	sendMsg(t, cm5, protoCall{
		Type: "call", ID: "dump-1", Topic: []string{"rpc", "hal", "dump"},
		Payload: json.RawMessage(`{}`), TimeoutMs: 1000,
	})
	sendMsg(t, cm5, protoPing{Type: "ping", TS: 77, SID: testCM5SID})

	type readResult struct {
		line []byte
		err  error
	}
	type wireHeader struct {
		Type string `json:"type"`
	}
	var gotReply, gotPong bool
	for i := 0; i < 2; i++ {
		msg := readMsg[wireHeader](t, cm5)
		switch msg.Type {
		case msgReply:
			gotReply = true
		case msgPong:
			gotPong = true
		default:
			t.Fatalf("unexpected message type %q", msg.Type)
		}
	}
	if !gotReply {
		t.Error("missing dump reply")
	}
	if !gotPong {
		t.Error("missing pong")
	}
}

func TestCallExport(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	reqConn := b.NewConnection("caller")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)
	unlockExports(t, cm5)

	type result struct {
		msg *bus.Message
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, err := reqConn.RequestWait(context.Background(), reqConn.NewMessage(
			bus.T("fabric", "out", "rpc", "hal", "dump"),
			map[string]string{"ask": "status"},
			false,
		))
		done <- result{msg: msg, err: err}
	}()

	call := readMsg[protoCall](t, cm5)
	if call.Type != "call" {
		t.Fatalf("expected call, got %q", call.Type)
	}
	want := []string{"rpc", "hal", "dump"}
	if !slicesEqual(call.Topic, want) {
		t.Fatalf("topic = %v, want %v", call.Topic, want)
	}
	var payload map[string]string
	if err := json.Unmarshal(call.Payload, &payload); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	if payload["ask"] != "status" {
		t.Fatalf("payload.ask = %q, want status", payload["ask"])
	}

	sendMsg(t, cm5, protoReply{
		Type:  "reply",
		Corr:  call.ID,
		OK:    true,
		Value: json.RawMessage(`{"ok":true,"remote":"cm5"}`),
	})

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("RequestWait: %v", res.err)
		}
		if res.msg == nil {
			t.Fatal("nil bus reply")
		}
		reply, ok := res.msg.Payload.(map[string]any)
		if !ok {
			t.Fatalf("payload type = %T, want map[string]any", res.msg.Payload)
		}
		if reply["remote"] != "cm5" {
			t.Fatalf("reply.remote = %#v", reply["remote"])
		}
		if reply["ok"] != true {
			t.Fatalf("reply.ok = %#v", reply["ok"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local reply")
	}
}

func TestCallExportOnlyConfiguredRule(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	reqConn := b.NewConnection("caller")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)
	unlockExports(t, cm5)

	// Use an unconfigured topic — only fabric/out/rpc/hal/dump is routed.
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer reqCancel()
	go func() {
		_, _ = reqConn.RequestWait(reqCtx, reqConn.NewMessage(
			bus.T("fabric", "out", "rpc", "hal", "not_configured"),
			map[string]string{"ask": "status"},
			false,
		))
	}()

	gotLine := make(chan struct{})
	go func() {
		_, _ = cm5.ReadLine()
		close(gotLine)
	}()

	select {
	case <-gotLine:
		t.Fatal("got wire call for unconfigured export rule")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestPendingWireCallsTimeout(t *testing.T) {
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	reqConn := b.NewConnection("caller")
	msg := reqConn.NewMessage(
		bus.T("fabric", "out", "rpc", "hal", "dump"),
		map[string]string{"ask": "status"},
		false,
	)
	sub := reqConn.Request(msg)
	defer reqConn.Unsubscribe(sub)

	s := session{
		conn: fabricConn,
		outboundCalls: []*outboundCall{
			{id: "wire-1", req: msg, deadline: time.Now().Add(-time.Millisecond)},
		},
	}

	s.drainOutbound(time.Now())

	select {
	case reply := <-sub.Channel():
		if reply == nil {
			t.Fatal("nil timeout reply")
		}
		out, ok := reply.Payload.(types.ErrorReply)
		if !ok {
			t.Fatalf("payload type = %T, want types.ErrorReply", reply.Payload)
		}
		if out.OK {
			t.Fatal("expected ok=false")
		}
		if out.Error != "timeout" {
			t.Fatalf("error = %q, want timeout", out.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for timeout reply")
	}
}

func TestDrainExportsDropsUnmarshalablePayload(t *testing.T) {
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}
	s := session{
		conn: fabricConn,
		tr:   tr,
		link: linkUp,
	}

	s.setupExports()
	defer s.teardownExports()

	pubConn.Publish(pubConn.NewMessage(bus.T("hal", "state"), make(chan int), false))
	s.drainExports()

	if len(tr.writes) != 0 {
		t.Fatalf("writes = %d, want 0", len(tr.writes))
	}
}

func TestDrainPendingCallsReportsMarshalFailure(t *testing.T) {
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	handlerConn := b.NewConnection("handler")
	tr := &captureTransport{}

	sub := handlerConn.Subscribe(bus.T("rpc", "hal", "dump"))
	defer handlerConn.Unsubscribe(sub)
	req := fabricConn.NewMessage(bus.T("rpc", "hal", "dump"), map[string]string{"ask": "status"}, false)
	replySub := fabricConn.Request(req)

	var msg *bus.Message
	select {
	case msg = <-sub.Channel():
		if msg == nil {
			t.Fatal("nil request message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for request message")
	}
	handlerConn.Reply(msg, make(chan int), false)

	s := session{
		conn: fabricConn,
		tr:   tr,
		inboundCalls: []*inboundCall{{
			id:       "call-1",
			sub:      replySub,
			deadline: time.Now().Add(time.Second),
		}},
	}

	s.drainInbound(time.Now())

	if len(tr.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(tr.writes))
	}
	var reply protoReply
	if err := json.Unmarshal(tr.writes[0], &reply); err != nil {
		t.Fatalf("Unmarshal reply: %v", err)
	}
	if reply.Corr != "call-1" {
		t.Fatalf("corr = %q, want call-1", reply.Corr)
	}
	if reply.OK {
		t.Fatal("expected ok=false")
	}
	if reply.Err != errPayloadMarshal {
		t.Fatalf("err = %q, want %q", reply.Err, errPayloadMarshal)
	}
}

func TestDrainOutgoingWireCallsReportsMarshalFailure(t *testing.T) {
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	reqConn := b.NewConnection("caller")
	tr := &captureTransport{}
	s := session{
		conn: fabricConn,
		tr:   tr,
		link: linkUp,
	}

	s.setupExports()
	defer s.teardownExports()

	msg := reqConn.NewMessage(
		bus.T("fabric", "out", "rpc", "hal", "dump"),
		make(chan int),
		false,
	)
	replySub := reqConn.Request(msg)
	defer reqConn.Unsubscribe(replySub)

	s.drainOutbound(time.Now())

	if len(tr.writes) != 0 {
		t.Fatalf("writes = %d, want 0", len(tr.writes))
	}
	if len(s.outboundCalls) != 0 {
		t.Fatalf("outboundCalls = %d, want 0", len(s.outboundCalls))
	}

	select {
	case reply := <-replySub.Channel():
		if reply == nil {
			t.Fatal("nil reply")
		}
		out, ok := reply.Payload.(types.ErrorReply)
		if !ok {
			t.Fatalf("payload type = %T, want types.ErrorReply", reply.Payload)
		}
		if out.OK {
			t.Fatal("expected ok=false")
		}
		if out.Error != errPayloadMarshal {
			t.Fatalf("error = %q, want %q", out.Error, errPayloadMarshal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for marshal failure reply")
	}
}

func TestDrainOutgoingWireCallsReportsWriteFailure(t *testing.T) {
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	reqConn := b.NewConnection("caller")
	tr := &captureTransport{writeErr: errors.New("boom")}
	s := session{
		conn: fabricConn,
		tr:   tr,
		link: linkUp,
	}

	s.setupExports()
	defer s.teardownExports()

	msg := reqConn.NewMessage(
		bus.T("fabric", "out", "rpc", "hal", "dump"),
		map[string]string{"ask": "status"},
		false,
	)
	replySub := reqConn.Request(msg)
	defer reqConn.Unsubscribe(replySub)

	s.drainOutbound(time.Now())

	if s.link != linkDown {
		t.Fatalf("link = %v, want %v", s.link, linkDown)
	}
	if len(s.outboundCalls) != 0 {
		t.Fatalf("outboundCalls = %d, want 0", len(s.outboundCalls))
	}

	select {
	case reply := <-replySub.Channel():
		if reply == nil {
			t.Fatal("nil reply")
		}
		out, ok := reply.Payload.(types.ErrorReply)
		if !ok {
			t.Fatalf("payload type = %T, want types.ErrorReply", reply.Payload)
		}
		if out.OK {
			t.Fatal("expected ok=false")
		}
		if out.Error != "transport_write_failed" {
			t.Fatalf("error = %q, want transport_write_failed", out.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for write failure reply")
	}
}

func TestCallExportPeerReset(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	reqConn := b.NewConnection("caller")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	bringUp(t, cm5)
	unlockExports(t, cm5)

	type result struct {
		msg *bus.Message
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, err := reqConn.RequestWait(context.Background(), reqConn.NewMessage(
			bus.T("fabric", "out", "rpc", "hal", "dump"),
			map[string]string{"ask": "status"},
			false,
		))
		done <- result{msg: msg, err: err}
	}()

	call := readMsg[protoCall](t, cm5)
	if call.Type != "call" {
		t.Fatalf("expected call, got %q", call.Type)
	}

	sendMsg(t, cm5, protoHello{
		Type: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "fresh-session", Proto: protoVersion,
	})
	_ = readMsg[protoHelloAck](t, cm5)

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("RequestWait: %v", res.err)
		}
		if res.msg == nil {
			t.Fatal("nil bus reply")
		}
		out, ok := res.msg.Payload.(types.ErrorReply)
		if !ok {
			t.Fatalf("payload type = %T, want types.ErrorReply", res.msg.Payload)
		}
		if out.OK {
			t.Fatal("expected ok=false")
		}
		if out.Error != "session_reset" {
			t.Fatalf("error = %q, want session_reset", out.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for peer-reset reply")
	}
}

func TestEchoedHelloAckIgnoredDuringOutgoingCall(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	reqConn := b.NewConnection("caller")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local", DefaultLinkConfig())
	ack := bringUp(t, cm5)
	unlockExports(t, cm5)

	type result struct {
		msg *bus.Message
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, err := reqConn.RequestWait(context.Background(), reqConn.NewMessage(
			bus.T("fabric", "out", "rpc", "hal", "dump"),
			map[string]string{"ask": "status"},
			false,
		))
		done <- result{msg: msg, err: err}
	}()

	call := readMsg[protoCall](t, cm5)
	if call.Type != "call" {
		t.Fatalf("expected call, got %q", call.Type)
	}

	// Send an echoed hello_ack (our own SID) — should be ignored.
	sendMsg(t, cm5, protoHelloAck{
		Type: "hello_ack", Node: "mcu-1", SID: ack.SID, Proto: protoVersion, OK: true,
	})

	sendMsg(t, cm5, protoReply{
		Type:  "reply",
		Corr:  call.ID,
		OK:    true,
		Value: json.RawMessage(`{"ok":true,"remote":"cm5"}`),
	})

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("RequestWait: %v", res.err)
		}
		if res.msg == nil {
			t.Fatal("nil bus reply")
		}
		reply, ok := res.msg.Payload.(map[string]any)
		if !ok {
			t.Fatalf("payload type = %T, want map[string]any", res.msg.Payload)
		}
		if reply["remote"] != "cm5" || reply["ok"] != true {
			t.Fatalf("unexpected reply payload: %#v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local reply after echoed hello_ack")
	}
}

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

func pipePair() (*RWTransport, *RWTransport) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	return NewRWTransport(r2, w1), NewRWTransport(r1, w2)
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

func bringUp(t *testing.T, cm5 Transport) wireHelloAck {
	t.Helper()
	sendMsg(t, cm5, wireHello{
		T: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s1", Proto: protoVersion,
	})
	ack := readMsg[wireHelloAck](t, cm5)
	if !ack.OK || ack.Node != "mcu-1" || ack.SID == "" || ack.Proto != protoVersion {
		t.Fatalf("bad hello_ack: %+v", ack)
	}
	time.Sleep(50 * time.Millisecond)
	return ack
}

func unlockExports(t *testing.T, cm5 Transport, sid string) {
	t.Helper()
	sendMsg(t, cm5, wirePing{T: "ping", TS: 77, SID: sid})
	pong := readMsg[wirePong](t, cm5)
	if pong.T != "pong" {
		t.Fatalf("expected pong, got %q", pong.T)
	}
}

// ---- codec ----

func TestCodecRoundTrip(t *testing.T) {
	orig := wireHello{T: "hello", Node: "mcu-1", Peer: "cm5-local", SID: "abc", Proto: protoVersion}
	data := marshal(orig)
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Error("marshal should end with newline")
	}
	jsonPart := data[:len(data)-1]
	if bytes.Contains(jsonPart, []byte("\n")) {
		t.Error("JSON should not contain embedded newlines")
	}
	if wireType(jsonPart) != "hello" {
		t.Errorf("wireType = %q", wireType(jsonPart))
	}
	var dec wireHello
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
		{wireHello{T: "hello"}, "hello"},
		{wireHelloAck{T: "hello_ack"}, "hello_ack"},
		{wirePing{T: "ping", TS: 1}, "ping"},
		{wirePong{T: "pong", TS: 2}, "pong"},
		{wirePub{T: "pub", Topic: []string{"a"}}, "pub"},
		{wireUnretain{T: "unretain", Topic: []string{"a"}}, "unretain"},
		{wireCall{T: "call", ID: "c1"}, "call"},
		{wireReply{T: "reply", Corr: "c1", OK: true}, "reply"},
	} {
		b := marshal(tc.v)
		if got := wireType(b[:len(b)-1]); got != tc.want {
			t.Errorf("wireType = %q, want %q", got, tc.want)
		}
	}
}

func TestWireTypeBadInput(t *testing.T) {
	for _, b := range [][]byte{[]byte("not json"), []byte(`{"no_t":true}`), nil} {
		if got := wireType(b); got != "" {
			t.Errorf("wireType(%q) = %q, want empty", b, got)
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
		if string(line) != `{"t":"ping","ts":99}` {
			t.Errorf("got %q", line)
		}
	}()
	sendMsg(t, a, wirePing{T: "ping", TS: 99})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestOversizeLineRecovery(t *testing.T) {
	big := `{"t":"ping","ts":0,"x":"` + strings.Repeat("x", maxLineLen+100) + `"}`
	input := big + "\n" + `{"t":"ping","ts":3}` + "\n"
	tr := NewRWTransport(strings.NewReader(input), io.Discard)
	_, err := tr.ReadLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
	line, err := tr.ReadLine()
	if err != nil {
		t.Fatalf("second ReadLine: %v", err)
	}
	if string(line) != `{"t":"ping","ts":3}` {
		t.Errorf("got %q", line)
	}
}

// ---- shmring transport ----

func TestShmringTransportRoundTrip(t *testing.T) {
	rx := shmring.New(256)
	tx := shmring.New(256)
	mcuTr := NewShmringTransport(rx, tx)
	defer mcuTr.Close()

	rx.TryWriteFrom([]byte(`{"t":"ping","ts":42}` + "\n"))
	line, err := mcuTr.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if string(line) != `{"t":"ping","ts":42}` {
		t.Errorf("got %q", line)
	}

	if err := mcuTr.WriteLine([]byte(`{"t":"pong","ts":42}`)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	var out [128]byte
	n := tx.TryReadInto(out[:])
	if string(out[:n]) != `{"t":"pong","ts":42}`+"\n" {
		t.Errorf("tx got %q", out[:n])
	}
}

func TestShmringTransportMultiLine(t *testing.T) {
	rx := shmring.New(256)
	tr := NewShmringTransport(rx, shmring.New(256))
	defer tr.Close()
	rx.TryWriteFrom([]byte(`{"t":"ping","ts":1}` + "\n" + `{"t":"ping","ts":2}` + "\n"))
	line1, _ := tr.ReadLine()
	line2, _ := tr.ReadLine()
	if string(line1) != `{"t":"ping","ts":1}` {
		t.Errorf("line1 = %q", line1)
	}
	if string(line2) != `{"t":"ping","ts":2}` {
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
	rx := shmring.New(4096)
	tr := NewShmringTransport(rx, shmring.New(256))
	defer tr.Close()
	big := make([]byte, maxLineLen+100)
	for i := range big {
		big[i] = 'x'
	}
	rx.TryWriteFrom(big)
	rx.TryWriteFrom([]byte("\n"))
	rx.TryWriteFrom([]byte(`{"t":"ping","ts":7}` + "\n"))
	_, err := tr.ReadLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
	line, err := tr.ReadLine()
	if err != nil {
		t.Fatalf("second ReadLine: %v", err)
	}
	if string(line) != `{"t":"ping","ts":7}` {
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")

	sendMsg(t, cm5, wireHello{
		T: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s1", Proto: protoVersion,
	})
	ack := readMsg[wireHelloAck](t, cm5)
	if !ack.OK || ack.Node != "mcu-1" || ack.SID == "" || ack.Proto != protoVersion {
		t.Errorf("bad ack: %+v", ack)
	}
	time.Sleep(50 * time.Millisecond)
	sendMsg(t, cm5, wirePing{T: "ping", TS: 99, SID: "s1"})
	pong := readMsg[wirePong](t, cm5)
	if pong.TS != 99 || pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestSessionReset(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")
	bringUp(t, cm5)

	sendMsg(t, cm5, wireHello{T: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s2", Proto: protoVersion})
	ack := readMsg[wireHelloAck](t, cm5)
	if !ack.OK || ack.SID == "" || ack.Proto != protoVersion {
		t.Error("hello_ack.OK = false")
	}
	sendMsg(t, cm5, wirePing{T: "ping", TS: 55, SID: "s2"})
	pong := readMsg[wirePong](t, cm5)
	if pong.TS != 55 || pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestRejectsWrongPeer(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")

	sendMsg(t, cm5, wireHello{T: "hello", Node: "cm5-local", Peer: "mcu-999", SID: "s1", Proto: protoVersion})
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
	sendMsg(t, cm5, wireHello{T: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s2", Proto: protoVersion})
	select {
	case res := <-gotLine:
		if res.err != nil {
			t.Fatalf("ReadLine error: %v", res.err)
		}
		var ack wireHelloAck
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")

	gotLine := make(chan readResult, 1)
	go func() {
		line, err := cm5.ReadLine()
		gotLine <- readResult{line: line, err: err}
	}()

	sendMsg(t, cm5, wireHello{T: "hello", Peer: "mcu-1", SID: "s1", Proto: protoVersion})
	select {
	case <-gotLine:
		t.Fatal("got response to hello without node")
	case <-time.After(200 * time.Millisecond):
	}

	sendMsg(t, cm5, wireHello{T: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "s2", Proto: protoVersion})
	select {
	case res := <-gotLine:
		if res.err != nil {
			t.Fatalf("ReadLine error: %v", res.err)
		}
		var ack wireHelloAck
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")
	ack := bringUp(t, cm5)
	sendMsg(t, cm5, wirePing{T: "ping", TS: 42, SID: "s1"})
	pong := readMsg[wirePong](t, cm5)
	if pong.TS != 42 || pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestMCUNeverInitiates(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")
	gotLine := make(chan struct{})
	go func() { cm5.ReadLine(); close(gotLine) }()
	select {
	case <-gotLine:
		t.Fatal("MCU sent unsolicited message")
	case <-time.After(2 * time.Second):
	}
	cancel()
}

func TestUnknownTypeIgnored(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")
	bringUp(t, cm5)
	cm5.WriteLine([]byte(`{"t":"future_msg"}`))
	sendMsg(t, cm5, wirePing{T: "ping", TS: 1})
	pong := readMsg[wirePong](t, cm5)
	if pong.TS != 1 {
		t.Errorf("pong.TS = %d", pong.TS)
	}
}

func TestMalformedJSONIgnored(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")
	bringUp(t, cm5)
	cm5.WriteLine([]byte("not json"))
	sendMsg(t, cm5, wirePing{T: "ping", TS: 2})
	pong := readMsg[wirePong](t, cm5)
	if pong.TS != 2 {
		t.Errorf("pong.TS = %d", pong.TS)
	}
}

func TestCancelClosesCleanly(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local"); close(done) }()
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")

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
		{[]string{"config", "device"}, "config/device"},
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
		{[]string{"rpc", "hal", "dump"}, "rpc/hal/dump"},
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
	go Run(ctx, mcu, conn, "mcu-1", "cm5-local")
	bringUp(t, cm5)

	reader := b.NewConnection("test")
	sub := reader.Subscribe(bus.T("config", "device"))

	sendMsg(t, cm5, wirePub{
		T:       "pub",
		Topic:   []string{"config", "device"},
		Payload: json.RawMessage(`{"mode":"normal"}`),
		Retain:  false,
	})

	select {
	case m := <-sub.Channel():
		if m == nil {
			t.Fatal("nil message")
		}
		raw, ok := m.Payload.(json.RawMessage)
		if !ok {
			t.Fatalf("payload type = %T, want json.RawMessage", m.Payload)
		}
		if string(raw) != `{"mode":"normal"}` {
			t.Errorf("payload = %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for imported pub")
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
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	ack := bringUp(t, cm5)
	unlockExports(t, cm5, ack.SID)

	publishConn.Publish(publishConn.NewMessage(
		bus.T("hal", "cap", "env", "temperature", "core", "value"),
		map[string]int{"deci_c": 412},
		true,
	))

	msg := readMsg[wirePub](t, cm5)
	if msg.T != "pub" {
		t.Fatalf("expected pub, got %q", msg.T)
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
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	ack := bringUp(t, cm5)
	unlockExports(t, cm5, ack.SID)

	// Publish retained value first.
	publishConn.Publish(publishConn.NewMessage(
		bus.T("hal", "cap", "env", "temperature", "core", "value"),
		map[string]int{"deci_c": 412},
		true,
	))
	pub := readMsg[wirePub](t, cm5)
	if pub.T != "pub" || !pub.Retain {
		t.Fatalf("expected retained pub, got t=%q retain=%v", pub.T, pub.Retain)
	}

	// Clear retained state (retain=true, payload=nil).
	publishConn.Publish(publishConn.NewMessage(
		bus.T("hal", "cap", "env", "temperature", "core", "value"),
		nil,
		true,
	))
	unr := readMsg[wireUnretain](t, cm5)
	if unr.T != "unretain" {
		t.Fatalf("expected unretain, got %q", unr.T)
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")

	sendMsg(t, cm5, wirePub{
		T: "pub", Topic: []string{"config", "device"},
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")

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

	sendMsg(t, cm5, wireUnretain{T: "unretain", Topic: []string{"config", "device"}})
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
	go Run(ctx, mcu, conn, "mcu-1", "cm5-local")
	bringUp(t, cm5)

	sendMsg(t, cm5, wirePub{
		T: "pub", Topic: []string{"config", "device"},
		Payload: json.RawMessage(`{"v":1}`), Retain: true,
	})
	time.Sleep(50 * time.Millisecond)
	sendMsg(t, cm5, wireUnretain{T: "unretain", Topic: []string{"config", "device"}})
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
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")

	handler := b.NewConnection("handler")
	sub := handler.Subscribe(bus.T("rpc", "hal", "dump"))
	defer handler.Unsubscribe(sub)

	sendMsg(t, cm5, wireCall{
		T: "call", ID: "pre-hello-1", Topic: []string{"rpc", "hal", "dump"},
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
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	bringUp(t, cm5)

	handler := b.NewConnection("handler")
	sub := handler.Subscribe(bus.T("rpc", "hal", "dump"))
	go func() {
		for m := range sub.Channel() {
			handler.Reply(m, map[string]string{"result": "ok"}, false)
		}
	}()

	sendMsg(t, cm5, wireCall{
		T: "call", ID: "test-corr-1", Topic: []string{"rpc", "hal", "dump"},
		Payload: json.RawMessage(`{}`), TimeoutMs: 5000,
	})

	reply := readMsg[wireReply](t, cm5)
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")
	bringUp(t, cm5)

	sendMsg(t, cm5, wireCall{
		T: "call", ID: "no-route-1", Topic: []string{"unknown", "endpoint"},
		Payload: json.RawMessage(`{}`), TimeoutMs: 1000,
	})

	reply := readMsg[wireReply](t, cm5)
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

func TestCallHandlerError(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	bringUp(t, cm5)

	handler := b.NewConnection("handler")
	sub := handler.Subscribe(bus.T("rpc", "hal", "dump"))
	go func() {
		for m := range sub.Channel() {
			handler.Reply(m, struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}{OK: false, Error: "device_busy"}, false)
		}
	}()

	sendMsg(t, cm5, wireCall{
		T: "call", ID: "err-1", Topic: []string{"rpc", "hal", "dump"},
		Payload: json.RawMessage(`{}`), TimeoutMs: 5000,
	})

	reply := readMsg[wireReply](t, cm5)
	if reply.Corr != "err-1" {
		t.Errorf("corr = %q", reply.Corr)
	}
	if reply.OK {
		t.Error("expected ok=false for handler error")
	}
	if reply.Err != "device_busy" {
		t.Errorf("err = %q, want device_busy", reply.Err)
	}
}

func TestCallDoesNotBlockPing(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	bringUp(t, cm5)

	handler := b.NewConnection("handler")
	sub := handler.Subscribe(bus.T("rpc", "hal", "dump"))
	go func() {
		for m := range sub.Channel() {
			time.Sleep(300 * time.Millisecond)
			handler.Reply(m, map[string]string{"result": "ok"}, false)
		}
	}()

	sendMsg(t, cm5, wireCall{
		T: "call", ID: "slow-1", Topic: []string{"rpc", "hal", "dump"},
		Payload: json.RawMessage(`{}`), TimeoutMs: 1000,
	})
	sendMsg(t, cm5, wirePing{T: "ping", TS: 77, SID: "s1"})

	type readResult struct {
		line []byte
		err  error
	}
	first := make(chan readResult, 1)
	go func() {
		line, err := cm5.ReadLine()
		first <- readResult{line: line, err: err}
	}()

	select {
	case res := <-first:
		if res.err != nil {
			t.Fatalf("ReadLine: %v", res.err)
		}
		if got := wireType(res.line); got != "pong" {
			t.Fatalf("first response type = %q, want pong", got)
		}
		var pong wirePong
		if err := json.Unmarshal(res.line, &pong); err != nil {
			t.Fatalf("Unmarshal pong: %v", err)
		}
		if pong.TS != 77 || pong.SID == "" {
			t.Fatalf("bad pong: %+v", pong)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("ping blocked behind slow call")
	}

	reply := readMsg[wireReply](t, cm5)
	if reply.Corr != "slow-1" {
		t.Errorf("corr = %q", reply.Corr)
	}
	if !reply.OK {
		t.Errorf("reply not ok: %s", reply.Err)
	}
}

func TestCallExport(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	reqConn := b.NewConnection("caller")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	ack := bringUp(t, cm5)
	unlockExports(t, cm5, ack.SID)

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

	call := readMsg[wireCall](t, cm5)
	if call.T != "call" {
		t.Fatalf("expected call, got %q", call.T)
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

	sendMsg(t, cm5, wireReply{
		T:       "reply",
		Corr:    call.ID,
		OK:      true,
		Payload: json.RawMessage(`{"ok":true,"remote":"cm5"}`),
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
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	ack := bringUp(t, cm5)
	unlockExports(t, cm5, ack.SID)

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

	s.drainOutboundPending(time.Now())

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
	var reply wireReply
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

	s.drainOutboundNew(time.Now())

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

	s.drainOutboundNew(time.Now())

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
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	ack := bringUp(t, cm5)
	unlockExports(t, cm5, ack.SID)

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

	call := readMsg[wireCall](t, cm5)
	if call.T != "call" {
		t.Fatalf("expected call, got %q", call.T)
	}

	sendMsg(t, cm5, wireHello{
		T: "hello", Node: "cm5-local", Peer: "mcu-1", SID: "fresh-session", Proto: protoVersion,
	})
	_ = readMsg[wireHelloAck](t, cm5)

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
		if out.Error != "peer_session_changed" {
			t.Fatalf("error = %q, want peer_session_changed", out.Error)
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
	go Run(ctx, mcu, fabricConn, "mcu-1", "cm5-local")
	ack := bringUp(t, cm5)
	unlockExports(t, cm5, ack.SID)

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

	call := readMsg[wireCall](t, cm5)
	if call.T != "call" {
		t.Fatalf("expected call, got %q", call.T)
	}

	sendMsg(t, cm5, wireHelloAck{
		T: "hello_ack", Node: "mcu-1", SID: ack.SID, Proto: protoVersion, OK: true,
	})

	sendMsg(t, cm5, wireReply{
		T:       "reply",
		Corr:    call.ID,
		OK:      true,
		Payload: json.RawMessage(`{"ok":true,"remote":"cm5"}`),
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

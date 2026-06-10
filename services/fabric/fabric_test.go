package fabric

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/x/shmring"
)

func pipePair() (*rwTransport, *rwTransport) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	return newRWTransport(r2, w1), newRWTransport(r1, w2)
}

func newBus() *bus.Bus { return bus.NewBus(3, "+", "#") }

func TestShouldLogFabricReadSkipsIdleDataFrames(t *testing.T) {
	longRead := 3 * time.Second
	longGap := 3 * time.Second

	for _, msgType := range []string{msgPub, msgPing, msgPong, msgUnretain} {
		if shouldLogFabricRead(msgType, longRead, longGap) {
			t.Fatalf("idle %s frame should not be logged", msgType)
		}
	}

	for _, msgType := range []string{msgHello, msgHelloAck, msgCall, msgReply, msgXferBegin, msgXferCommit, msgXferAbort} {
		if !shouldLogFabricRead(msgType, 0, 0) {
			t.Fatalf("control %s frame should be logged", msgType)
		}
	}
}

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
		Type: "hello", Proto: protocolName, Node: "bigbox-cm5", SID: testCM5SID,
	})
	ack := readMsg[protoHelloAck](t, cm5)
	if ack.Node != "mcu" || ack.SID == "" || ack.Proto != protocolName {
		t.Fatalf("bad hello_ack: %+v", ack)
	}
	time.Sleep(50 * time.Millisecond)
	return ack
}

func unlockExports(t *testing.T, cm5 Transport) {
	t.Helper()
	sendMsg(t, cm5, protoPing{Type: "ping", SID: testCM5SID})
	pong := readMsg[protoPong](t, cm5)
	if pong.Type != "pong" {
		t.Fatalf("expected pong, got %q", pong.Type)
	}
}

// ---- codec ----

func TestCodecRoundTrip(t *testing.T) {
	orig := protoHello{Type: "hello", Proto: protocolName, Node: "bigbox-cm5", SID: "abc"}
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
	if dec.Type != orig.Type || dec.Proto != orig.Proto || dec.Node != orig.Node || dec.SID != orig.SID {
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
		{protoPing{Type: "ping"}, "ping"},
		{protoPong{Type: "pong"}, "pong"},
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
		if string(line) != `{"type":"ping","sid":"s1"}` {
			t.Errorf("got %q", line)
		}
	}()
	sendMsg(t, a, protoPing{Type: "ping", SID: "s1"})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestOversizeLineRecovery(t *testing.T) {
	big := `{"type":"test","n":0,"x":"` + strings.Repeat("x", maxLineLen+100) + `"}`
	input := big + "\n" + `{"type":"test","n":3}` + "\n"
	tr := newRWTransport(strings.NewReader(input), io.Discard)
	_, err := tr.ReadLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
	line, err := tr.ReadLine()
	if err != nil {
		t.Fatalf("second ReadLine: %v", err)
	}
	if string(line) != `{"type":"test","n":3}` {
		t.Errorf("got %q", line)
	}
}

func TestReleaseTransferChunkFitsLineLimit(t *testing.T) {
	raw := bytes.Repeat([]byte{'x'}, int(DefaultLinkConfig().MaxAcceptedChunkSize))
	line := marshal(protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-line-limit",
		Offset:      0,
		Data:        base64.RawURLEncoding.EncodeToString(raw),
		ChunkDigest: "00000000",
	})
	if got := len(line) - 1; got > maxLineLen {
		t.Fatalf("%d-byte raw transfer chunk frame len = %d, max %d", len(raw), got, maxLineLen)
	}
}

// ---- shmring transport ----

func TestShmringTransportRoundTrip(t *testing.T) {
	rx := shmring.New(256)
	tx := shmring.New(256)
	mcuTr := NewShmringTransport(rx, tx)
	defer mcuTr.Close()

	rx.TryWriteFrom([]byte(`{"type":"test","n":42}` + "\n"))
	line, err := mcuTr.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if string(line) != `{"type":"test","n":42}` {
		t.Errorf("got %q", line)
	}

	if err := mcuTr.WriteLine([]byte(`{"type":"test","n":42}`)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	var out [128]byte
	n := tx.TryReadInto(out[:])
	if string(out[:n]) != `{"type":"test","n":42}`+"\n" {
		t.Errorf("tx got %q", out[:n])
	}
}

func TestShmringTransportMultiLine(t *testing.T) {
	rx := shmring.New(256)
	tr := NewShmringTransport(rx, shmring.New(256))
	defer tr.Close()
	rx.TryWriteFrom([]byte(`{"type":"test","n":1}` + "\n" + `{"type":"test","n":2}` + "\n"))
	line1, _ := tr.ReadLine()
	line2, _ := tr.ReadLine()
	if string(line1) != `{"type":"test","n":1}` {
		t.Errorf("line1 = %q", line1)
	}
	if string(line2) != `{"type":"test","n":2}` {
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
	// Ring must be larger than maxLineLen+100 + newline + the trailing test
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
	rx.TryWriteFrom([]byte(`{"type":"test","n":7}` + "\n"))
	_, err := tr.ReadLine()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
	line, err := tr.ReadLine()
	if err != nil {
		t.Fatalf("second ReadLine: %v", err)
	}
	if string(line) != `{"type":"test","n":7}` {
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())

	sendMsg(t, cm5, protoHello{
		Type: "hello", Proto: protocolName, Node: "bigbox-cm5", SID: "s1",
	})
	ack := readMsg[protoHelloAck](t, cm5)
	if ack.Node != "mcu" || ack.SID == "" || ack.Proto != protocolName {
		t.Errorf("bad ack: %+v", ack)
	}
	time.Sleep(50 * time.Millisecond)
	sendMsg(t, cm5, protoPing{Type: "ping", SID: "s1"})
	pong := readMsg[protoPong](t, cm5)
	if pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestSessionReset(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	sendMsg(t, cm5, protoHello{Type: "hello", Proto: protocolName, Node: "bigbox-cm5", SID: "s2"})
	ack := readMsg[protoHelloAck](t, cm5)
	if ack.SID == "" || ack.Proto != protocolName {
		t.Errorf("bad hello_ack: %+v", ack)
	}
	sendMsg(t, cm5, protoPing{Type: "ping", SID: "s2"})
	pong := readMsg[protoPong](t, cm5)
	if pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestDuplicateSameSIDHelloRefreshesWithoutReset(t *testing.T) {
	tr := &captureTransport{}
	sink := &fakeTransferSink{}
	s := session{
		tr:       tr,
		link:     linkUp,
		localSID: "mcu-sid-test",
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		peerSID:  "s1",
		peerNode: "bigbox-cm5",
		incomingTransfer: &incomingTransfer{
			meta:   transferMeta{ID: "xfer-1"},
			worker: newTransferSinkWorker("xfer-1", sink),
		},
	}

	s.onHello(&protoHello{Type: msgHello, Proto: protocolName, Node: "bigbox-cm5", SID: "s1"})

	if len(tr.writes) != 1 {
		t.Fatalf("hello_ack writes = %d, want 1", len(tr.writes))
	}
	var ack protoHelloAck
	if err := json.Unmarshal(tr.writes[0], &ack); err != nil {
		t.Fatalf("hello_ack decode failed: %v", err)
	}
	if ack.Type != msgHelloAck || ack.SID != "mcu-sid-test" || ack.Node != "mcu" {
		t.Fatalf("bad hello_ack: %+v", ack)
	}
	if s.incomingTransfer == nil || len(sink.abortReasons) != 0 {
		t.Fatalf("same-SID hello reset transfer: incoming=%v aborts=%v", s.incomingTransfer != nil, sink.abortReasons)
	}
	if s.peerSID != "s1" || s.peerNode != "bigbox-cm5" {
		t.Fatalf("peer identity changed incorrectly: sid=%q node=%q", s.peerSID, s.peerNode)
	}
}

func TestDuplicateSameSIDHelloAckRefreshesWithoutReset(t *testing.T) {
	tr := &captureTransport{}
	sink := &fakeTransferSink{}
	s := session{
		tr:       tr,
		link:     linkUp,
		localSID: "mcu-sid-test",
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		peerSID:  "s1",
		peerNode: "bigbox-cm5",
		incomingTransfer: &incomingTransfer{
			meta:   transferMeta{ID: "xfer-1"},
			worker: newTransferSinkWorker("xfer-1", sink),
		},
	}

	s.onHelloAck(&protoHelloAck{Type: msgHelloAck, Proto: protocolName, Node: "bigbox-cm5", SID: "s1"})

	if len(tr.writes) != 0 {
		t.Fatalf("hello_ack refresh wrote %d frames, want 0", len(tr.writes))
	}
	if s.incomingTransfer == nil || len(sink.abortReasons) != 0 {
		t.Fatalf("same-SID hello_ack reset transfer: incoming=%v aborts=%v", s.incomingTransfer != nil, sink.abortReasons)
	}
	if s.peerSID != "s1" || s.peerNode != "bigbox-cm5" {
		t.Fatalf("peer identity changed incorrectly: sid=%q node=%q", s.peerSID, s.peerNode)
	}
}

func TestRejectsWrongNode(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())

	sendMsg(t, cm5, protoHello{Type: "hello", Proto: protocolName, Node: "cm5-wrong", SID: "s1"})
	gotLine := make(chan readResult, 1)
	go func() {
		line, err := cm5.ReadLine()
		gotLine <- readResult{line: line, err: err}
	}()
	select {
	case <-gotLine:
		t.Fatal("got response to wrong-node hello")
	case <-time.After(200 * time.Millisecond):
	}
	sendMsg(t, cm5, protoHello{Type: "hello", Proto: protocolName, Node: "bigbox-cm5", SID: "s2"})
	select {
	case res := <-gotLine:
		if res.err != nil {
			t.Fatalf("ReadLine error: %v", res.err)
		}
		var ack protoHelloAck
		if err := json.Unmarshal(res.line, &ack); err != nil {
			t.Fatalf("expected hello_ack: %v", err)
		}
		if ack.Proto != protocolName {
			t.Fatalf("bad hello_ack: %+v", ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no hello_ack for correct peer")
	}
}

func TestRejectsWrongNodeHelloAck(t *testing.T) {
	s := session{peerID: "bigbox-cm5"}
	s.onHelloAck(&protoHelloAck{
		Type:  msgHelloAck,
		Proto: protocolName,
		Node:  "cm5-wrong",
		SID:   "s1",
	})

	if s.link == linkUp || s.peerSID != "" || s.peerNode != "" {
		t.Fatalf("wrong-node hello_ack changed session: link=%v peer_sid=%q peer_node=%q", s.link, s.peerSID, s.peerNode)
	}
}

func TestRejectsMissingNodeWhenPeerPinned(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())

	gotLine := make(chan readResult, 1)
	go func() {
		line, err := cm5.ReadLine()
		gotLine <- readResult{line: line, err: err}
	}()

	sendMsg(t, cm5, protoHello{Type: "hello", Proto: protocolName, SID: "s1"})
	select {
	case <-gotLine:
		t.Fatal("got response to hello without node")
	case <-time.After(200 * time.Millisecond):
	}

	sendMsg(t, cm5, protoHello{Type: "hello", Proto: protocolName, Node: "bigbox-cm5", SID: "s2"})
	select {
	case res := <-gotLine:
		if res.err != nil {
			t.Fatalf("ReadLine error: %v", res.err)
		}
		var ack protoHelloAck
		if err := json.Unmarshal(res.line, &ack); err != nil {
			t.Fatalf("expected hello_ack: %v", err)
		}
		if ack.Proto != protocolName {
			t.Fatalf("bad hello_ack: %+v", ack)
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	ack := bringUp(t, cm5)
	sendMsg(t, cm5, protoPing{Type: "ping", SID: "s1"})
	pong := readMsg[protoPong](t, cm5)
	if pong.SID != ack.SID {
		t.Errorf("bad pong: %+v ack=%+v", pong, ack)
	}
}

func TestEchoedPingIgnored(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	ack := bringUp(t, cm5)

	sendMsg(t, cm5, protoPing{Type: "ping", SID: ack.SID})
	sendMsg(t, cm5, protoPing{Type: "ping", SID: testCM5SID})

	pong := readMsg[protoPong](t, cm5)
	if pong.SID != ack.SID {
		t.Errorf("bad pong after echoed ping: %+v ack=%+v", pong, ack)
	}
}

func TestEchoedTransferControlIgnored(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	sendMsg(t, cm5, protoXferNeed{Type: msgXferNeed, XferID: "echoed", Next: 0})
	sendMsg(t, cm5, protoPing{Type: "ping", SID: testCM5SID})

	pong := readMsg[protoPong](t, cm5)
	if pong.Type != msgPong {
		t.Errorf("bad pong after echoed transfer control: %+v", pong)
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", LinkConfig{PingInterval: 150 * time.Millisecond})
	bringUp(t, cm5)

	for i := 0; i < 3; i++ {
		ping := readMsg[protoPing](t, cm5)
		if ping.Type != msgPing {
			t.Fatalf("ping[%d] type = %q, want %q", i, ping.Type, msgPing)
		}
	}
}

func TestReadyHeldUntilExportHoldoff(t *testing.T) {
	// The Go side gates rpcReady on exportReadyAt elapsing post-handshake.
	mcu, cm5 := pipePair()
	b := newBus()
	observer := b.NewConnection("observer")
	sub := observer.Subscribe(bus.T("state", "fabric", "link", "mcu-uart0"))
	defer observer.Unsubscribe(sub)
	publisher := b.NewConnection("publisher")
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-new", "boot_id": "boot-new"},
		true,
	))
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "self", "updater"),
		map[string]string{"state": "running"},
		true,
	))
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "self", "health"),
		map[string]string{"state": "ok"},
		true,
	))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)
	go func() {
		for i := 0; i < len(criticalExportTopics); i++ {
			_, _ = cm5.ReadLine()
		}
	}()

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
	// importPublishRules is empty in the production contract, so this test
	// installs a scoped temp rule. The mechanism under test is the generic
	// retain-tracking + session-reset teardown chain, not the specific topic.
	prev := importPublishRules
	importPublishRules = append([]importRule{}, prev...)
	importPublishRules = append(importPublishRules, importRule{
		wire:  []string{"test", "wire", "config"},
		local: []string{"test", "local", "config"},
	})
	t.Cleanup(func() { importPublishRules = prev })
	cfgTopic := bus.T("test", "local", "config")

	mcu, cm5 := pipePair()
	b := newBus()
	observer := b.NewConnection("observer")
	cfgSub := observer.Subscribe(cfgTopic)
	defer observer.Unsubscribe(cfgSub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	// Push a payload via the temp import path so the local topic
	// becomes a tracked imported retain.
	sendMsg(t, cm5, protoPub{
		Type:    msgPub,
		Topic:   []string{"test", "wire", "config"},
		Payload: json.RawMessage(`{"hello":"world"}`),
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
			t.Fatal("timeout waiting for initial imported retain")
		}
	}

	// Force a session reset: hello with a new SID. Concurrent reader
	// drains the new hello_ack the MCU sends back; pipePair is
	// synchronous so without this the MCU's sendControl would block,
	// promoteLink would never fire, and teardownImportedRetained would
	// not run.
	go func() { _ = readMsg[protoHelloAck](t, cm5) }()
	sendMsg(t, cm5, protoHello{
		Type:  msgHello,
		Proto: protocolName,
		Node:  "bigbox-cm5",
		SID:   "cm5-sid-new",
	})

	// Expect a nil-payload retained publish on the imported topic.
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
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
		Type:  msgHello,
		Proto: protocolName,
		Node:  "bigbox-cm5",
		SID:   "cm5-sid-new",
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", LinkConfig{MaxInboundHelpers: 1})
	bringUp(t, cm5)

	// First call holds the only helper slot. The bus has no handler, so
	// the call sits as a pending request until timeout.
	sendMsg(t, cm5, protoCall{
		Type:    msgCall,
		ID:      "c1",
		Topic:   []string{"rpc", "test", "noop"},
		Payload: json.RawMessage(`{}`),
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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)
	cm5.WriteLine([]byte(`{"type":"future_msg"}`))
	sendMsg(t, cm5, protoPing{Type: "ping", SID: testCM5SID})
	pong := readMsg[protoPong](t, cm5)
	if pong.Type != msgPong {
		t.Errorf("bad pong: %+v", pong)
	}
}

func TestMalformedJSONIgnored(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)
	cm5.WriteLine([]byte("not json"))
	sendMsg(t, cm5, protoPing{Type: "ping", SID: testCM5SID})
	pong := readMsg[protoPong](t, cm5)
	if pong.Type != msgPong {
		t.Errorf("bad pong: %+v", pong)
	}
}

func TestCancelClosesCleanly(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
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
	sub := observer.Subscribe(bus.T("state", "fabric", "link", "mcu-uart0"))
	defer observer.Unsubscribe(sub)
	publisher := b.NewConnection("publisher")
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-new", "boot_id": "boot-new"},
		true,
	))
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "self", "updater"),
		map[string]string{"state": "running"},
		true,
	))
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "self", "health"),
		map[string]string{"state": "ok"},
		true,
	))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())

	ack := bringUp(t, cm5)
	go func() {
		for i := 0; i < len(criticalExportTopics); i++ {
			_, _ = cm5.ReadLine()
		}
	}()

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
				if payload.LinkID != "mcu-uart0" {
					t.Fatalf("link_id = %q, want mcu-uart0", payload.LinkID)
				}
				if !payload.Ready || !payload.Established {
					t.Fatalf("expected ready/established link state, got %+v", payload)
				}
				if payload.PeerID != "bigbox-cm5" {
					t.Fatalf("peer_id = %q, want bigbox-cm5", payload.PeerID)
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
	// importPublishRules is empty. Anything queried returns nil.
	for _, tc := range [][]string{
		{"config", "device"}, // legacy gone
		{"config", "other"},
		{"unknown", "x"},
		nil,
	} {
		if got := importPublishTopic(tc); got != nil {
			t.Errorf("importPublishTopic(%v) = %v, want nil", tc, got)
		}
	}
}

func TestImportCallTopic(t *testing.T) {
	// The current Lua migration wire surface uses cap/self/updater/main/rpc/*.
	for _, tc := range []struct {
		wire []string
		want string
	}{
		{[]string{"cap", "self", "updater", "main", "rpc", "prepare-update"}, "rpc/updater/prepare"},
		{[]string{"cap", "self", "updater", "main", "rpc", "commit-update"}, "rpc/updater/commit"},
		{[]string{"cmd", "self", "updater", "prepare"}, ""},
		{[]string{"cmd", "self", "updater", "commit"}, ""},
		{[]string{"rpc", "hal", "dump"}, ""},
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
	// The current wire surface exports state/self/* and event/self/* only.
	for _, tc := range []struct {
		bus  bus.Topic
		want []string
	}{
		{bus.T("state", "self", "software"), []string{"state", "self", "software"}},
		{bus.T("state", "self", "power", "battery"), []string{"state", "self", "power", "battery"}},
		{bus.T("event", "self", "power", "charger", "alert"), []string{"event", "self", "power", "charger", "alert"}},
		{bus.T("hal", "cap", "env", "temperature", "core", "value"), nil}, // legacy gone
		{bus.T("hal", "state"), nil}, // legacy gone
		{bus.T("other", "topic"), nil},
	} {
		got := exportTopic(tc.bus)
		if tc.want == nil {
			if got != nil {
				t.Errorf("exportTopic(%v) = %v, want nil", tc.bus, got)
			}
		} else if !slicesEqual(got, tc.want) {
			t.Errorf("exportTopic(%v) = %v, want %v", tc.bus, got, tc.want)
		}
	}
}

func TestExportCallTopic(t *testing.T) {
	// exportCallRules is empty; the MCU does not originate outbound RPC calls.
	if got := exportCallTopic(bus.T("fabric", "out", "rpc", "hal", "dump")); got != nil {
		t.Errorf("exportCallTopic(legacy dump path) = %v, want nil", got)
	}
}

func TestExportCallPatterns(t *testing.T) {
	patterns := exportCallPatterns()
	if len(patterns) != 0 {
		t.Fatalf("len(exportCallPatterns()) = %d, want 0", len(patterns))
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

// ---- pub export ----

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

func TestDrainExportsContinuesDuringIncomingTransfer(t *testing.T) {
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}
	s := session{
		conn:             fabricConn,
		tr:               tr,
		link:             linkUp,
		exportsEnabled:   true,
		incomingTransfer: &incomingTransfer{},
	}

	s.setupExports()
	defer s.teardownExports()

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-new", "boot_id": "boot-new"},
		true,
	))
	s.drainExports()

	if len(tr.writes) != 1 {
		t.Fatalf("writes during transfer = %d, want 1", len(tr.writes))
	}
}

func TestDrainExportsContinuesAfterPrepareCall(t *testing.T) {
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}
	cfg := DefaultLinkConfig()
	s := session{
		conn:           fabricConn,
		tr:             tr,
		cfg:            cfg,
		link:           linkUp,
		exportsEnabled: true,
		exportReadyAt:  time.Now().Add(-time.Second),
	}

	s.setupExports()
	defer s.teardownExports()
	defer s.teardownInbound()

	s.onCall(&protoCall{
		Type:  msgCall,
		ID:    "prepare-1",
		Topic: []string{"cap", "self", "updater", "main", "rpc", "prepare-update"},
	})

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-new", "boot_id": "boot-new"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "updater"),
		map[string]any{
			"state":            "ready",
			"pending_image_id": "mcu-dev-13.0",
			"job_id":           "job-1",
		},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "health"),
		map[string]string{"state": "ok"},
		true,
	))
	s.drainExports()

	if len(tr.writes) != 1 {
		t.Fatalf("writes after prepare call = %d, want 1", len(tr.writes))
	}
}

func TestDrainExportsDoesNotUsePostTransferQuietWindow(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}
	s := session{
		conn:           fabricConn,
		tr:             tr,
		link:           linkUp,
		exportsEnabled: true,
		exportReadyAt:  time.Now().Add(-time.Second),
	}

	s.setupExports()
	defer s.teardownExports()

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "runtime", "memory"),
		map[string]int{"alloc_bytes": 241376},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-new", "boot_id": "boot-new"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "updater"),
		map[string]string{"state": "rebooting"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "health"),
		map[string]string{"state": "ok"},
		true,
	))

	for i := 0; i < len(criticalExportTopics)+4; i++ {
		s.drainExports()
	}
	if len(tr.writes) < len(criticalExportTopics) {
		t.Fatalf("writes after transfer = %d, want at least %d critical facts",
			len(tr.writes), len(criticalExportTopics))
	}
	want := [][]string{
		{"state", "self", "software"},
		{"state", "self", "updater"},
		{"state", "self", "health"},
	}
	for i, topic := range want {
		pub := decodePubWrite(t, tr.writes[i])
		if !slicesEqual(pub.Topic, topic) {
			t.Fatalf("write %d topic = %v, want %v", i, pub.Topic, topic)
		}
	}
}

func TestDrainExportsPrioritizesCriticalRetainedFacts(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "runtime", "memory"),
		map[string]int{"alloc_bytes": 241376},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "health"),
		map[string]string{"state": "ok"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "updater"),
		map[string]string{"state": "running"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-new", "boot_id": "boot-new"},
		true,
	))

	s := session{
		conn:           fabricConn,
		tr:             tr,
		link:           linkUp,
		exportsEnabled: true,
		exportReadyAt:  time.Now().Add(-time.Second),
	}
	s.setupExports()
	defer s.teardownExports()

	for i := 0; i < 3; i++ {
		s.drainExports()
	}
	if len(tr.writes) != 3 {
		t.Fatalf("writes after critical drains = %d, want 3", len(tr.writes))
	}

	want := [][]string{
		{"state", "self", "software"},
		{"state", "self", "updater"},
		{"state", "self", "health"},
	}
	for i, topic := range want {
		pub := decodePubWrite(t, tr.writes[i])
		if !slicesEqual(pub.Topic, topic) {
			t.Fatalf("write %d topic = %v, want %v", i, pub.Topic, topic)
		}
		if !pub.Retain {
			t.Fatalf("write %d retain = false, want true", i)
		}
	}

	for i := 0; i < 8; i++ {
		s.drainExports()
	}
	counts := map[string]int{}
	for _, write := range tr.writes {
		pub := decodePubWrite(t, write)
		counts[wireTopicString(pub.Topic)]++
	}
	for _, topic := range want {
		key := wireTopicString(topic)
		if counts[key] != 1 {
			t.Fatalf("critical topic %s sent %d times, want exactly once", key, counts[key])
		}
	}
	if counts["state/self/runtime/memory"] != 1 {
		t.Fatalf("telemetry topic sent %d times, want once", counts["state/self/runtime/memory"])
	}
}

func TestDrainCriticalExportsCoalescesLatestRetainedFact(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "old", "boot_id": "boot-old"},
		true,
	))

	s := session{
		conn:           fabricConn,
		tr:             tr,
		link:           linkUp,
		exportsEnabled: true,
		exportReadyAt:  time.Now().Add(-time.Second),
	}
	s.setupExports()
	defer s.teardownExports()

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "new", "boot_id": "boot-new"},
		true,
	))

	s.drainExports()
	if len(tr.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(tr.writes))
	}
	pub := decodePubWrite(t, tr.writes[0])
	if !slicesEqual(pub.Topic, []string{"state", "self", "software"}) {
		t.Fatalf("topic = %v, want state/self/software", pub.Topic)
	}
	var payload map[string]string
	if err := json.Unmarshal(pub.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["image_id"] != "new" || payload["boot_id"] != "boot-new" {
		t.Fatalf("payload = %+v, want newest software fact", payload)
	}
}

func TestDrainExportsCoalescesQueuedRetainedTelemetry(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}
	sub := fabricConn.Subscribe(bus.T("state", "self", "runtime", "#"))
	defer fabricConn.Unsubscribe(sub)

	s := session{
		conn:           fabricConn,
		tr:             tr,
		link:           linkUp,
		exportsEnabled: true,
		exportReadyAt:  time.Now().Add(-time.Second),
		exportSubs:     []*bus.Subscription{sub},
	}

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "runtime", "memory"),
		map[string]int{"seq": 1},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "runtime", "memory"),
		map[string]int{"seq": 2},
		true,
	))

	s.drainExports()
	if len(tr.writes) != 1 {
		t.Fatalf("writes = %d, want one coalesced retained export", len(tr.writes))
	}
	pub := decodePubWrite(t, tr.writes[0])
	if !slicesEqual(pub.Topic, []string{"state", "self", "runtime", "memory"}) {
		t.Fatalf("topic = %v, want state/self/runtime/memory", pub.Topic)
	}
	var payload map[string]int
	if err := json.Unmarshal(pub.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["seq"] != 2 {
		t.Fatalf("payload seq = %d, want latest seq 2", payload["seq"])
	}
}

func TestReadyWaitsForQueuedCriticalReplayAdmission(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	watchConn := b.NewConnection("watch")
	linkSub := watchConn.Subscribe(bus.T("state", "fabric", "link", defaultLinkID))
	defer watchConn.Unsubscribe(linkSub)
	tr := &captureTransport{}

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-new", "boot_id": "boot-new"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "updater"),
		map[string]string{"state": "idle"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "health"),
		map[string]string{"state": "ok"},
		true,
	))

	s := session{
		linkID:         defaultLinkID,
		peerID:         "mcu",
		localSID:       "mcu-sid",
		peerSID:        "cm5-sid",
		conn:           fabricConn,
		tr:             tr,
		link:           linkUp,
		exportsEnabled: true,
		exportReadyAt:  time.Now().Add(-time.Second),
	}
	s.setupExports()
	defer s.teardownExports()

	s.tickReady(time.Now())
	if s.rpcReady {
		t.Fatal("rpcReady raised before critical replay drain")
	}
	if _, ok := readLinkState(linkSub); ok {
		t.Fatal("link state published before critical replay drain")
	}

	for i := 0; i < len(criticalExportTopics)-1; i++ {
		s.drainExports()
		s.tickReady(time.Now())
		if s.rpcReady {
			t.Fatalf("rpcReady raised after %d critical writes, want still false", i+1)
		}
		if _, ok := readLinkState(linkSub); ok {
			t.Fatalf("link state published after %d critical writes, want none", i+1)
		}
	}

	s.drainExports()
	s.tickReady(time.Now())
	if !s.rpcReady {
		t.Fatal("rpcReady did not raise after critical replay drain")
	}
	state, ok := readLinkState(linkSub)
	if !ok {
		t.Fatal("missing ready link state publish")
	}
	if !state.Ready || state.Status != statusReady {
		t.Fatalf("link state = %+v, want ready", state)
	}
	if len(tr.writes) != len(criticalExportTopics) {
		t.Fatalf("critical writes = %d, want %d", len(tr.writes), len(criticalExportTopics))
	}
}

func TestReadyBlocksWhenCriticalReplayFactsAreAbsentAndSuppressesTelemetry(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "runtime", "memory"),
		map[string]int{"alloc_bytes": 241376},
		true,
	))

	s := session{
		linkID:         defaultLinkID,
		peerID:         "mcu",
		localSID:       "mcu-sid",
		peerSID:        "cm5-sid",
		conn:           fabricConn,
		tr:             tr,
		link:           linkUp,
		exportsEnabled: true,
		exportReadyAt:  time.Now().Add(-time.Second),
	}
	s.setupExports()
	defer s.teardownExports()

	s.tickReady(time.Now())
	if s.rpcReady {
		t.Fatal("rpcReady raised before critical replay drain")
	}
	for i := 0; i < 3; i++ {
		s.drainExports()
		s.tickReady(time.Now())
	}
	if s.rpcReady {
		t.Fatal("rpcReady raised after absent critical replay facts")
	}
	if len(tr.writes) != 0 {
		t.Fatalf("writes = %d, want no telemetry while critical replay is absent", len(tr.writes))
	}
}

func TestLateCriticalExportsDrainBeforeWildcardTelemetryAndReady(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	fabricConn := b.NewConnection("fabric")
	pubConn := b.NewConnection("publisher")
	tr := &captureTransport{}

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "runtime", "memory"),
		map[string]int{"alloc_bytes": 241376},
		true,
	))

	s := session{
		linkID:         defaultLinkID,
		peerID:         "mcu",
		localSID:       "mcu-sid",
		peerSID:        "cm5-sid",
		conn:           fabricConn,
		tr:             tr,
		link:           linkUp,
		exportsEnabled: true,
		exportReadyAt:  time.Now().Add(-time.Second),
	}
	s.setupExports()
	defer s.teardownExports()

	s.drainExports()
	s.tickReady(time.Now())
	if s.rpcReady {
		t.Fatal("rpcReady raised before initial critical replay facts")
	}
	if len(tr.writes) != 0 {
		t.Fatalf("initial writes = %d, want no telemetry before critical replay", len(tr.writes))
	}
	start := len(tr.writes)

	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "runtime", "cpu"),
		map[string]int{"load_pct": 42},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "health"),
		map[string]string{"state": "ok"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "updater"),
		map[string]string{"state": "running"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-new", "boot_id": "boot-new"},
		true,
	))

	wantCritical := [][]string{
		{"state", "self", "software"},
		{"state", "self", "updater"},
		{"state", "self", "health"},
	}
	for i, topic := range wantCritical {
		s.drainExports()
		pub := decodePubWrite(t, tr.writes[start+i])
		if !slicesEqual(pub.Topic, topic) {
			t.Fatalf("post-ready write %d topic = %v, want %v", i, pub.Topic, topic)
		}
		s.tickReady(time.Now())
		if i < len(wantCritical)-1 && s.rpcReady {
			t.Fatalf("rpcReady raised after %d critical writes, want still false", i+1)
		}
	}
	if !s.rpcReady {
		t.Fatal("rpcReady did not raise after all critical facts were exported")
	}

	for i := 0; i < 8; i++ {
		s.drainExports()
	}
	counts := map[string]int{}
	for _, write := range tr.writes[start:] {
		pub := decodePubWrite(t, write)
		counts[wireTopicString(pub.Topic)]++
	}
	for _, topic := range wantCritical {
		key := wireTopicString(topic)
		if counts[key] != 1 {
			t.Fatalf("post-ready critical topic %s sent %d times, want exactly once", key, counts[key])
		}
	}
	if counts["state/self/runtime/cpu"] != 1 {
		t.Fatalf("post-ready telemetry sent %d times, want once", counts["state/self/runtime/cpu"])
	}

	start = len(tr.writes)
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "runtime", "temperature"),
		map[string]int{"deci_c": 421},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "health"),
		map[string]string{"state": "ok", "reason": "ready-edge"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "updater"),
		map[string]string{"state": "idle"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		bus.T("state", "self", "software"),
		map[string]string{"image_id": "mcu-newer", "boot_id": "boot-newer"},
		true,
	))
	for i, topic := range wantCritical {
		s.drainExports()
		pub := decodePubWrite(t, tr.writes[start+i])
		if !slicesEqual(pub.Topic, topic) {
			t.Fatalf("post-ready write %d topic = %v, want %v", i, pub.Topic, topic)
		}
	}
	for i := 0; i < 8; i++ {
		s.drainExports()
	}
	counts = map[string]int{}
	for _, write := range tr.writes[start:] {
		pub := decodePubWrite(t, write)
		counts[wireTopicString(pub.Topic)]++
	}
	for _, topic := range wantCritical {
		key := wireTopicString(topic)
		if counts[key] != 1 {
			t.Fatalf("post-ready critical topic %s sent %d times, want exactly once", key, counts[key])
		}
	}
	if counts["state/self/runtime/temperature"] != 1 {
		t.Fatalf("post-ready telemetry sent %d times, want once", counts["state/self/runtime/temperature"])
	}
}

func decodePubWrite(t *testing.T, line []byte) protoPub {
	t.Helper()
	var pub protoPub
	if err := json.Unmarshal(line, &pub); err != nil {
		t.Fatalf("Unmarshal pub %q: %v", line, err)
	}
	if pub.Type != msgPub {
		t.Fatalf("frame type = %q, want %q", pub.Type, msgPub)
	}
	return pub
}

func readLinkState(sub *bus.Subscription) (linkStatePayload, bool) {
	select {
	case msg, ok := <-sub.Channel():
		if !ok || msg == nil {
			return linkStatePayload{}, false
		}
		state, ok := msg.Payload.(linkStatePayload)
		return state, ok
	default:
		return linkStatePayload{}, false
	}
}

func TestPongAllowedDuringIncomingTransfer(t *testing.T) {
	tr := &captureTransport{}
	s := session{
		tr:       tr,
		link:     linkUp,
		localSID: "mcu-sid-test",
		peerSID:  "cm5-sid",
		incomingTransfer: &incomingTransfer{
			meta: transferMeta{ID: "xfer-1"},
		},
	}

	s.onPing(&protoPing{Type: msgPing, SID: "cm5-sid"})

	if len(tr.writes) != 1 {
		t.Fatalf("pong writes during transfer = %d, want 1", len(tr.writes))
	}
	var pong protoPong
	if err := json.Unmarshal(tr.writes[0], &pong); err != nil {
		t.Fatalf("pong decode failed: %v", err)
	}
	if pong.Type != msgPong || pong.SID != "mcu-sid-test" {
		t.Fatalf("bad pong: %+v", pong)
	}
}

func TestPongAllowedForEstablishedPeerWithoutQuietWindow(t *testing.T) {
	tr := &captureTransport{}
	s := session{
		tr:       tr,
		link:     linkUp,
		localSID: "mcu-sid-test",
		peerSID:  "cm5-sid",
	}

	s.onPing(&protoPing{Type: msgPing, SID: "cm5-sid"})

	if len(tr.writes) != 1 {
		t.Fatalf("pong writes = %d, want 1", len(tr.writes))
	}
	var pong protoPong
	if err := json.Unmarshal(tr.writes[0], &pong); err != nil {
		t.Fatalf("pong decode failed: %v", err)
	}
	if pong.Type != msgPong || pong.SID != "mcu-sid-test" {
		t.Fatalf("bad pong: %+v", pong)
	}
}

func TestPongRejectsWrongSIDWithoutQuietWindow(t *testing.T) {
	tr := &captureTransport{}
	s := session{
		tr:       tr,
		link:     linkUp,
		localSID: "mcu-sid-test",
		peerSID:  "cm5-sid",
	}

	s.onPing(&protoPing{Type: msgPing, SID: "other-sid"})

	if len(tr.writes) != 0 {
		t.Fatalf("pong writes for wrong sid = %d, want 0", len(tr.writes))
	}
}

func TestWrongSIDPingPongDoNotRefreshLiveness(t *testing.T) {
	tr := &captureTransport{}
	oldRx := time.Now().Add(-time.Hour)
	s := session{
		tr:       tr,
		link:     linkUp,
		localSID: "mcu-sid-test",
		peerSID:  "cm5-sid",
		lastRxAt: oldRx,
	}

	s.dispatch(marshal(protoPing{Type: msgPing, SID: "other-sid"}))
	if !s.lastRxAt.Equal(oldRx) {
		t.Fatalf("wrong-sid ping refreshed liveness: got %v want %v", s.lastRxAt, oldRx)
	}
	if len(tr.writes) != 0 {
		t.Fatalf("pong writes for wrong sid = %d, want 0", len(tr.writes))
	}

	s.dispatch(marshal(protoPong{Type: msgPong, SID: "other-sid"}))
	if !s.lastRxAt.Equal(oldRx) {
		t.Fatalf("wrong-sid pong refreshed liveness: got %v want %v", s.lastRxAt, oldRx)
	}

	s.dispatch(marshal(protoPong{Type: msgPong, SID: "cm5-sid"}))
	if !s.lastRxAt.After(oldRx) {
		t.Fatalf("current peer pong did not refresh liveness: got %v old %v", s.lastRxAt, oldRx)
	}
}

func TestPongRejectsSelfSIDWithoutQuietWindow(t *testing.T) {
	tr := &captureTransport{}
	s := session{
		tr:       tr,
		link:     linkUp,
		localSID: "mcu-sid-test",
		peerSID:  "mcu-sid-test",
	}

	s.onPing(&protoPing{Type: msgPing, SID: "mcu-sid-test"})

	if len(tr.writes) != 0 {
		t.Fatalf("pong writes for self sid = %d, want 0", len(tr.writes))
	}
}

// ---- unretain ----

func TestPubIgnoredBeforeHandshake(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())

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
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())

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

// ---- call import ----

func TestCallIgnoredBeforeHandshake(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu", "bigbox-cm5", DefaultLinkConfig())

	handler := b.NewConnection("handler")
	sub := handler.Subscribe(bus.T("rpc", "hal", "dump"))
	defer handler.Unsubscribe(sub)

	sendMsg(t, cm5, protoCall{
		Type: "call", ID: "pre-hello-1", Topic: []string{"rpc", "hal", "dump"},
		Payload: json.RawMessage(`{}`),
	})

	select {
	case m := <-sub.Channel():
		t.Fatalf("unexpected pre-handshake call dispatch: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCallImport(t *testing.T) {
	// Test the canonical inbound call route: cap/self/updater/main/rpc/prepare-update
	// maps to local rpc/updater/prepare where services/updater binds.
	diag := captureOTADiag(t)
	mcu, cm5 := pipePair()
	b := newBus()
	fabricConn := b.NewConnection("fabric")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, fabricConn, "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	handler := b.NewConnection("handler")
	sub := handler.Subscribe(bus.T("rpc", "updater", "prepare"))
	go func() {
		for m := range sub.Channel() {
			handler.Reply(m, map[string]string{"result": "ok"}, false)
		}
	}()

	sendMsg(t, cm5, protoCall{
		Type: "call", ID: "test-corr-1", Topic: []string{"cap", "self", "updater", "main", "rpc", "prepare-update"},
		Payload: json.RawMessage(`{"job_id":"job-prepare","expected_image_id":"mcu-dev-15.3"}`),
	})

	reply := readMsg[protoReply](t, cm5)
	if reply.Corr != "test-corr-1" {
		t.Errorf("corr = %q", reply.Corr)
	}
	if !reply.OK {
		t.Errorf("reply not ok: %s", reply.Err)
	}
	lines := diag.snapshot()
	assertDiagContains(t, lines, "[fabric-rpc]", "ev call_rx", "call_id test-corr-1", "job_id job-prepare", "expected_image_id mcu-dev-15.3")
	assertDiagContains(t, lines, "[fabric-rpc]", "ev call_route_ok", "local_topic rpc/updater/prepare")
	assertDiagContains(t, lines, "[fabric-rpc]", "ev call_dispatch_start")
	waitDiagContains(t, diag, "[fabric-rpc]", "ev call_reply_tx", "ok true", "sent true")
}

func TestCallNoRoute(t *testing.T) {
	mcu, cm5 := pipePair()
	b := newBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	sendMsg(t, cm5, protoCall{
		Type: "call", ID: "no-route-1", Topic: []string{"unknown", "endpoint"},
		Payload: json.RawMessage(`{}`),
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

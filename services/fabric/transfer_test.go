package fabric

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/updater"
	"devicecode-go/x/xxhash"
)

type fakeTransferSink struct {
	offs         []uint32
	writes       [][]byte
	writeErr     error
	commitErr    error
	applyErr     error
	commitInfo   transferInfo
	committed    bool
	applied      bool
	abortReasons []string
}

func (s *fakeTransferSink) WriteChunk(off uint32, data []byte) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.offs = append(s.offs, off)
	s.writes = append(s.writes, append([]byte(nil), data...))
	return nil
}

func (s *fakeTransferSink) Commit() (transferInfo, error) {
	if s.commitErr != nil {
		return transferInfo{}, s.commitErr
	}
	s.committed = true
	return s.commitInfo, nil
}

func (s *fakeTransferSink) Apply() error {
	s.applied = true
	return s.applyErr
}

func (s *fakeTransferSink) Abort(reason string) error {
	s.abortReasons = append(s.abortReasons, reason)
	return nil
}

// Bytes returns nil because the test fake doesn't retain a RAM copy
// of the transferred bytes — it tracks per-chunk writes instead.
func (s *fakeTransferSink) Bytes() []byte { return nil }

func runSessionWithSink(ctx context.Context, tr Transport, conn *bus.Connection, sink *fakeTransferSink) {
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       tr,
		conn:     conn,
		beginTransfer: func(meta transferMeta) (transferSink, error) {
			return sink, nil
		},
	}
	s.run(ctx)
}

func rawURL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// xxhashStr is the wire-format digest: lower-case hex, 8 chars. Mirrors
// the Lua reference's M.digest_hex.
func xxhashStr(data []byte) string {
	return xxhashHex(xxhash.Sum32(data, 0))
}

func xferBegin(id string, payload []byte, meta json.RawMessage) protoXferBegin {
	return protoXferBegin{
		Type:      msgXferBegin,
		XferID:    id,
		Target:    updater.TargetUpdaterMain,
		Size:      uint32(len(payload)),
		DigestAlg: updater.DigestAlgXXHash32,
		Digest:    xxhashStr(payload),
		Meta:      meta,
	}
}

func xferChunk(id string, off uint32, payload []byte) protoXferChunk {
	return protoXferChunk{
		Type:        msgXferChunk,
		XferID:      id,
		Offset:      off,
		Data:        rawURL(payload),
		ChunkDigest: xxhashStr(payload),
	}
}

func xferCommit(id string, payload []byte) protoXferCommit {
	return protoXferCommit{
		Type:      msgXferCommit,
		XferID:    id,
		Size:      uint32(len(payload)),
		DigestAlg: updater.DigestAlgXXHash32,
		Digest:    xxhashStr(payload),
	}
}

func installStageResponder(t *testing.T, b *bus.Bus, reply updater.StageReply) <-chan updater.StagePayload {
	t.Helper()
	conn := b.NewConnection("test-stage")
	sub := conn.Subscribe(updater.TopicStageRPC)
	t.Cleanup(func() { conn.Unsubscribe(sub) })
	got := make(chan updater.StagePayload, 4)
	go func() {
		for msg := range sub.Channel() {
			if msg == nil {
				continue
			}
			if payload, ok := msg.Payload.(updater.StagePayload); ok {
				select {
				case got <- payload:
				default:
				}
			}
			conn.Reply(msg, reply, false)
		}
	}()
	return got
}

func readTransferReady(t *testing.T, tr Transport, id string, next uint32) {
	t.Helper()
	ready := readMsg[protoXferReady](t, tr)
	if ready.Type != msgXferReady || ready.XferID != id {
		t.Fatalf("bad xfer_ready: %+v", ready)
	}
	need := readMsg[protoXferNeed](t, tr)
	if need.Type != msgXferNeed || need.XferID != id || need.Next != next {
		t.Fatalf("bad initial xfer_need: %+v, want id=%s next=%d", need, id, next)
	}
}

func TestTransferBeginPreservesMeta(t *testing.T) {
	// xfer_begin's meta is opaque to fabric-protocol but must be preserved
	// for updater/main staging diagnostics.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var captured transferMeta
	sink := &fakeTransferSink{}
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		beginTransfer: func(meta transferMeta) (transferSink, error) {
			captured = meta
			return sink, nil
		},
	}
	go s.run(ctx)
	bringUp(t, cm5)

	payload := []byte("abcd")
	metaBlob := json.RawMessage(`{"version":"1.2.3"}`)

	sendMsg(t, cm5, xferBegin("xfer-meta", payload, metaBlob))
	readTransferReady(t, cm5, "xfer-meta", 0)

	if string(captured.Meta) != string(metaBlob) {
		t.Fatalf("transferMeta.Meta = %q, want %q", captured.Meta, metaBlob)
	}
	if captured.ID != "xfer-meta" || captured.Size != uint32(len(payload)) {
		t.Fatalf("transferMeta basic fields wrong: %+v", captured)
	}
	if captured.Target != updater.TargetUpdaterMain || captured.DigestAlg != updater.DigestAlgXXHash32 || captured.Digest != xxhashStr(payload) {
		t.Fatalf("transferMeta contract fields wrong: %+v", captured)
	}
}

func TestTransferDuplicateBeginResendsReady(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	begin := xferBegin("xfer-dup", payload, nil)

	sendMsg(t, cm5, begin)
	readTransferReady(t, cm5, "xfer-dup", 0)

	sendMsg(t, cm5, begin)
	readTransferReady(t, cm5, "xfer-dup", 0)
}

func TestTransferReceiveSuccess(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{
		commitInfo: transferInfo{
			BytesWritten: 10,
			SlotXIPAddr:  0x10280000,
		},
	}
	stageCalls := installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})

	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcdefghij")
	sendMsg(t, cm5, xferBegin("xfer-2", payload, nil))
	readTransferReady(t, cm5, "xfer-2", 0)

	parts := [][]byte{payload[:4], payload[4:8], payload[8:]}
	off := uint32(0)
	for i, part := range parts {
		sendMsg(t, cm5, xferChunk("xfer-2", off, part))
		need := readMsg[protoXferNeed](t, cm5)
		want := off + uint32(len(part))
		if need.Next != want {
			t.Fatalf("xfer_need[%d].next = %d, want %d", i, need.Next, want)
		}
		off = want
	}

	sendMsg(t, cm5, xferCommit("xfer-2", payload))

	done := readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != "xfer-2" {
		t.Fatalf("bad xfer_done: %+v", done)
	}

	select {
	case call := <-stageCalls:
		if call.XferID != "xfer-2" || call.Target != updater.TargetUpdaterMain || call.Digest != xxhashStr(payload) {
			t.Fatalf("stage payload wrong: %+v", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stage call")
	}

	if got := string(sink.writes[0]) + string(sink.writes[1]) + string(sink.writes[2]); got != string(payload) {
		t.Fatalf("sink writes = %q, want %q", got, payload)
	}
	if !sink.committed {
		t.Fatal("sink.Commit was not called")
	}
	if sink.applied {
		t.Fatal("sink.Apply should not be called by strict target staging")
	}
}

func TestTransferChunkFutureOffsetRequestsCurrentAndCompletes(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-future-offset", payload, nil))
	readTransferReady(t, cm5, "xfer-future-offset", 0)

	sendMsg(t, cm5, xferChunk("xfer-future-offset", 7, payload))

	need := readMsg[protoXferNeed](t, cm5)
	if need.Type != msgXferNeed || need.XferID != "xfer-future-offset" || need.Next != 0 {
		t.Fatalf("future offset retry xfer_need = %+v, want next=0", need)
	}
	if len(sink.writes) != 0 {
		t.Fatalf("sink received %d writes, want 0", len(sink.writes))
	}
	if len(sink.abortReasons) != 0 {
		t.Fatalf("sink.Abort called on recoverable future offset: %v", sink.abortReasons)
	}

	sendMsg(t, cm5, xferChunk("xfer-future-offset", 0, payload))
	need = readMsg[protoXferNeed](t, cm5)
	if need.Next != uint32(len(payload)) {
		t.Fatalf("xfer_need.next after recovery = %d, want %d", need.Next, len(payload))
	}

	sendMsg(t, cm5, xferCommit("xfer-future-offset", payload))
	done := readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != "xfer-future-offset" {
		t.Fatalf("bad xfer_done: %+v", done)
	}
	if got := string(sink.writes[0]); got != string(payload) {
		t.Fatalf("sink writes = %q, want %q", got, payload)
	}
	if !sink.committed {
		t.Fatal("sink.Commit was not called")
	}
}

func TestTransferChunkStaleOffsetRequestsCurrentAndCompletes(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcdef")
	sendMsg(t, cm5, xferBegin("xfer-stale-offset", payload, nil))
	readTransferReady(t, cm5, "xfer-stale-offset", 0)

	sendMsg(t, cm5, xferChunk("xfer-stale-offset", 0, []byte("abc")))
	need := readMsg[protoXferNeed](t, cm5)
	if need.Next != 3 {
		t.Fatalf("xfer_need.next after first chunk = %d, want 3", need.Next)
	}

	sendMsg(t, cm5, xferChunk("xfer-stale-offset", 0, []byte("abc")))
	need = readMsg[protoXferNeed](t, cm5)
	if need.Type != msgXferNeed || need.XferID != "xfer-stale-offset" || need.Next != 3 {
		t.Fatalf("stale offset retry xfer_need = %+v, want next=3", need)
	}
	if len(sink.writes) != 1 {
		t.Fatalf("sink received %d writes after stale duplicate, want 1", len(sink.writes))
	}
	if len(sink.abortReasons) != 0 {
		t.Fatalf("sink.Abort called on recoverable stale offset: %v", sink.abortReasons)
	}

	sendMsg(t, cm5, xferChunk("xfer-stale-offset", 3, []byte("def")))
	need = readMsg[protoXferNeed](t, cm5)
	if need.Next != uint32(len(payload)) {
		t.Fatalf("xfer_need.next after recovery = %d, want %d", need.Next, len(payload))
	}

	sendMsg(t, cm5, xferCommit("xfer-stale-offset", payload))
	done := readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != "xfer-stale-offset" {
		t.Fatalf("bad xfer_done: %+v", done)
	}
	if got := string(sink.writes[0]) + string(sink.writes[1]); got != string(payload) {
		t.Fatalf("sink writes = %q, want %q", got, payload)
	}
	if !sink.committed {
		t.Fatal("sink.Commit was not called")
	}
}

func TestTransferChunkDecodeFailureRequestsSameOffset(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-d1", payload, nil))
	readTransferReady(t, cm5, "xfer-d1", 0)

	// Bogus base64 (uses non-base64url chars).
	sendMsg(t, cm5, protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-d1",
		Offset:      0,
		Data:        "!!!not-base64!!!",
		ChunkDigest: xxhashStr(payload),
	})

	need := readMsg[protoXferNeed](t, cm5)
	if need.Type != msgXferNeed || need.XferID != "xfer-d1" || need.Next != 0 {
		t.Fatalf("bad retry xfer_need: %+v", need)
	}
	if len(sink.abortReasons) != 0 {
		t.Fatalf("sink.Abort called on recoverable decode failure: %v", sink.abortReasons)
	}
	if len(sink.writes) != 0 {
		t.Fatalf("sink received %d writes before decode passed", len(sink.writes))
	}

	sendMsg(t, cm5, xferChunk("xfer-d1", 0, payload))
	need = readMsg[protoXferNeed](t, cm5)
	if need.Next != uint32(len(payload)) {
		t.Fatalf("xfer_need.next after retry = %d, want %d", need.Next, len(payload))
	}
}

func TestTransferChunkMissingDigestAborts(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-missing-digest", payload, nil))
	readTransferReady(t, cm5, "xfer-missing-digest", 0)

	sendMsg(t, cm5, protoXferChunk{
		Type:   msgXferChunk,
		XferID: "xfer-missing-digest",
		Offset: 0,
		Data:   rawURL(payload),
	})

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Err != "missing_chunk_digest" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
	if len(sink.abortReasons) == 0 {
		t.Fatal("expected sink.Abort on missing chunk digest")
	}
}

func TestTransferChunkDigestMismatchRequestsSameOffset(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-bad-chunk-digest", payload, nil))
	readTransferReady(t, cm5, "xfer-bad-chunk-digest", 0)

	sendMsg(t, cm5, protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-bad-chunk-digest",
		Offset:      0,
		Data:        rawURL(payload),
		ChunkDigest: "00000000",
	})
	need := readMsg[protoXferNeed](t, cm5)
	if need.Next != 0 {
		t.Fatalf("retry xfer_need.next = %d, want 0", need.Next)
	}
	if len(sink.writes) != 0 {
		t.Fatalf("sink received %d writes before digest passed", len(sink.writes))
	}

	sendMsg(t, cm5, xferChunk("xfer-bad-chunk-digest", 0, payload))
	need = readMsg[protoXferNeed](t, cm5)
	if need.Next != uint32(len(payload)) {
		t.Fatalf("xfer_need.next after retry = %d, want %d", need.Next, len(payload))
	}
}

func TestTransferChunkSizeOverflowAborts(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	// Advertise size=4 but send 6 bytes in the first chunk.
	sendMsg(t, cm5, xferBegin("xfer-d2", payload, nil))
	readTransferReady(t, cm5, "xfer-d2", 0)

	sendMsg(t, cm5, protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-d2",
		Offset:      0,
		Data:        rawURL([]byte("abcdef")),
		ChunkDigest: xxhashStr([]byte("abcdef")),
	})

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Err != "size_too_large" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
}

func TestTransferCommitDigestMismatchAborts(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	// Begin with the wrong digest advertised. The streamed bytes disagree
	// with the begin/commit digest even though the frames agree.
	bogus := strings.Repeat("0", 8)
	sendMsg(t, cm5, protoXferBegin{
		Type:      msgXferBegin,
		XferID:    "xfer-4",
		Target:    updater.TargetUpdaterMain,
		Size:      uint32(len(payload)),
		DigestAlg: updater.DigestAlgXXHash32,
		Digest:    bogus,
	})
	readTransferReady(t, cm5, "xfer-4", 0)

	sendMsg(t, cm5, xferChunk("xfer-4", 0, payload))
	_ = readMsg[protoXferNeed](t, cm5)

	sendMsg(t, cm5, protoXferCommit{
		Type:      msgXferCommit,
		XferID:    "xfer-4",
		Size:      uint32(len(payload)),
		DigestAlg: updater.DigestAlgXXHash32,
		Digest:    bogus,
	})

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Type != msgXferAbort || abort.Err != "digest_mismatch" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
	if len(sink.abortReasons) == 0 {
		t.Fatal("expected sink abort on digest mismatch")
	}
}

// bufferingSinkAdapter wraps the production bufferSink so transfer tests
// can assert the bytes passed to updater/main staging.
type bufferingSinkAdapter struct {
	*bufferSink
	abortReasons []string
}

func (b *bufferingSinkAdapter) Abort(reason string) error {
	b.abortReasons = append(b.abortReasons, reason)
	return b.bufferSink.Abort(reason)
}

func TestTransferTargetInvokedAfterCommit(t *testing.T) {
	// With target=updater/main, fabric calls the local updater stage RPC
	// after xfer_commit and before xfer_done. The wire never names a
	// raw/member receiver topic.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gotPayload := installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})

	sink := &bufferingSinkAdapter{bufferSink: newBufferSink(transferMeta{Size: 4})}
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		beginTransfer: func(meta transferMeta) (transferSink, error) {
			sink.bufferSink.meta = meta
			return sink, nil
		},
	}
	go s.run(ctx)
	bringUp(t, cm5)

	payload := []byte("abcd")
	metaBlob := json.RawMessage(`{"version":"1.2.3"}`)

	sendMsg(t, cm5, xferBegin("xfer-stage", payload, metaBlob))
	readTransferReady(t, cm5, "xfer-stage", 0)

	sendMsg(t, cm5, xferChunk("xfer-stage", 0, payload))
	_ = readMsg[protoXferNeed](t, cm5)

	sendMsg(t, cm5, xferCommit("xfer-stage", payload))

	select {
	case p := <-gotPayload:
		if p.XferID != "xfer-stage" {
			t.Fatalf("stage xfer_id = %v, want xfer-stage", p.XferID)
		}
		if p.LinkID != defaultLinkID {
			t.Fatalf("stage link_id = %q, want %q", p.LinkID, defaultLinkID)
		}
		if p.Target != updater.TargetUpdaterMain || p.DigestAlg != updater.DigestAlgXXHash32 || p.Digest != xxhashStr(payload) {
			t.Fatalf("stage contract fields wrong: %+v", p)
		}
		if string(p.Artefact) != string(payload) {
			t.Fatalf("stage artefact = %v, want %q", p.Artefact, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stage call")
	}

	done := readMsg[protoXferDone](t, cm5)
	if done.XferID != "xfer-stage" {
		t.Fatalf("xfer_done xfer_id = %q, want xfer-stage", done.XferID)
	}
}

func TestTransferTargetRejectAbortsTransfer(t *testing.T) {
	// updater/main stage replies {ok=false, err=...}. fabric must send
	// xfer_abort with the stage reason rather than xfer_done.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = installStageResponder(t, b, updater.StageReply{OK: false, Err: "manifest_check_failed"})

	sink := &bufferingSinkAdapter{bufferSink: newBufferSink(transferMeta{Size: 4})}
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		beginTransfer: func(meta transferMeta) (transferSink, error) {
			sink.bufferSink.meta = meta
			return sink, nil
		},
	}
	go s.run(ctx)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-rej", payload, nil))
	readTransferReady(t, cm5, "xfer-rej", 0)
	sendMsg(t, cm5, xferChunk("xfer-rej", 0, payload))
	_ = readMsg[protoXferNeed](t, cm5)
	sendMsg(t, cm5, xferCommit("xfer-rej", payload))

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.XferID != "xfer-rej" {
		t.Fatalf("xfer_abort xfer_id = %q, want xfer-rej", abort.XferID)
	}
	if abort.Err != "manifest_check_failed" {
		t.Fatalf("xfer_abort err = %q, want manifest_check_failed", abort.Err)
	}
}

func TestTransferIdleChunkWatchdog(t *testing.T) {
	// transfer_mgr.lua refreshes active.deadline = now + phase_timeout on
	// each accepted chunk and aborts with reason="timeout" if the deadline
	// passes. With a tight PhaseTimeout, dropping the wire after xfer_begin
	// must produce an unsolicited xfer_abort within ~one drain tick.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		cfg:      LinkConfig{PhaseTimeout: 100 * time.Millisecond},
		beginTransfer: func(meta transferMeta) (transferSink, error) {
			return sink, nil
		},
	}
	go s.run(ctx)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-wd", payload, nil))
	readTransferReady(t, cm5, "xfer-wd", 0)

	// Stop sending chunks. The watchdog should resend the current offset a
	// bounded number of times before aborting, so a lost xfer_need does not
	// strand both sides until the first idle timeout.
	for i := 0; i < transferIdleRetryLimit; i++ {
		need := readMsg[protoXferNeed](t, cm5)
		if need.Type != msgXferNeed || need.XferID != "xfer-wd" || need.Next != 0 {
			t.Fatalf("bad retry xfer_need[%d]: %+v", i, need)
		}
	}
	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Type != msgXferAbort || abort.XferID != "xfer-wd" || abort.Err != "timeout" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
	if len(sink.abortReasons) == 0 || sink.abortReasons[0] != "timeout" {
		t.Fatalf("sink.Abort reasons = %v, want [\"timeout\"]", sink.abortReasons)
	}
}

func TestTransferCommitDigestMismatchOnCommitFrameAborts(t *testing.T) {
	// xfer_begin and xfer_commit must agree on the digest. If they
	// disagree (even when the streamed bytes match begin), commit aborts.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-5", payload, nil))
	readTransferReady(t, cm5, "xfer-5", 0)

	sendMsg(t, cm5, xferChunk("xfer-5", 0, payload))
	_ = readMsg[protoXferNeed](t, cm5)

	// Commit advertises a different digest than begin: must abort.
	sendMsg(t, cm5, protoXferCommit{
		Type:      msgXferCommit,
		XferID:    "xfer-5",
		Size:      uint32(len(payload)),
		DigestAlg: updater.DigestAlgXXHash32,
		Digest:    strings.Repeat("0", 8),
	})
	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Type != msgXferAbort || abort.Err != "digest_mismatch" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
}

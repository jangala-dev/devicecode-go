package fabric

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"devicecode-go/bus"
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

func runSessionWithSink(ctx context.Context, tr Transport, conn *bus.Connection, sink *fakeTransferSink) {
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu-1",
		peerID:   "cm5-local",
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

// xxhashStr is the wire-format checksum: lower-case hex, 8 chars, no algorithm
// field. Mirrors the Lua reference's M.digest_hex.
func xxhashStr(data []byte) string {
	return xxhashHex(xxhash.Sum32(data, 0))
}

func TestTransferBeginPreservesMeta(t *testing.T) {
	// xfer_begin's meta is opaque to fabric-protocol but must be preserved
	// for fabric-update's receiver, which pulls meta.receiver out of it.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var captured transferMeta
	sink := &fakeTransferSink{}
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu-1",
		peerID:   "cm5-local",
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
	metaBlob := json.RawMessage(`{"receiver":["raw","member","mcu","cap","updater","main","rpc","receive"],"version":"1.2.3"}`)

	sendMsg(t, cm5, protoXferBegin{
		Type:     msgXferBegin,
		XferID:   "xfer-meta",
		Size:     uint32(len(payload)),
		Checksum: xxhashStr(payload),
		Meta:     metaBlob,
	})
	_ = readMsg[protoXferReady](t, cm5)

	if string(captured.Meta) != string(metaBlob) {
		t.Fatalf("transferMeta.Meta = %q, want %q", captured.Meta, metaBlob)
	}
	if captured.ID != "xfer-meta" || captured.Size != uint32(len(payload)) {
		t.Fatalf("transferMeta basic fields wrong: %+v", captured)
	}
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

	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcdefghij")
	checksum := xxhashStr(payload)

	sendMsg(t, cm5, protoXferBegin{
		Type:     msgXferBegin,
		XferID:   "xfer-2",
		Size:     uint32(len(payload)),
		Checksum: checksum,
	})

	ready := readMsg[protoXferReady](t, cm5)
	if ready.Type != msgXferReady || ready.XferID != "xfer-2" {
		t.Fatalf("bad xfer_ready: %+v", ready)
	}

	parts := [][]byte{payload[:4], payload[4:8], payload[8:]}
	off := uint32(0)
	for i, part := range parts {
		sendMsg(t, cm5, protoXferChunk{
			Type:   msgXferChunk,
			XferID: "xfer-2",
			Offset: off,
			Data:   rawURL(part),
		})
		need := readMsg[protoXferNeed](t, cm5)
		want := off + uint32(len(part))
		if need.Next != want {
			t.Fatalf("xfer_need[%d].next = %d, want %d", i, need.Next, want)
		}
		off = want
	}

	sendMsg(t, cm5, protoXferCommit{
		Type:     msgXferCommit,
		XferID:   "xfer-2",
		Size:     uint32(len(payload)),
		Checksum: checksum,
	})

	done := readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != "xfer-2" {
		t.Fatalf("bad xfer_done: %+v", done)
	}

	time.Sleep(postTransferDoneSettle + 50*time.Millisecond)

	if got := string(sink.writes[0]) + string(sink.writes[1]) + string(sink.writes[2]); got != string(payload) {
		t.Fatalf("sink writes = %q, want %q", got, payload)
	}
	if !sink.committed {
		t.Fatal("sink.Commit was not called")
	}
	if !sink.applied {
		t.Fatal("sink.Apply was not called")
	}
}

func TestTransferChunkBadOffsetAborts(t *testing.T) {
	// Lua transfer_mgr aborts and clears the active transfer on chunk faults
	// (unexpected_offset, decode_failed, size_overflow). Match that — do not
	// keep the transfer alive with an xfer_need.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, protoXferBegin{
		Type:     msgXferBegin,
		XferID:   "xfer-3",
		Size:     uint32(len(payload)),
		Checksum: xxhashStr(payload),
	})
	_ = readMsg[protoXferReady](t, cm5)

	// Send a chunk at the wrong byte offset; expect xfer_abort and
	// sink.Abort, not an xfer_need retry.
	sendMsg(t, cm5, protoXferChunk{
		Type:   msgXferChunk,
		XferID: "xfer-3",
		Offset: 7,
		Data:   rawURL(payload),
	})

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Type != msgXferAbort || abort.XferID != "xfer-3" || abort.Err != "unexpected_offset" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
	if len(sink.writes) != 0 {
		t.Fatalf("sink received %d writes, want 0", len(sink.writes))
	}
	if len(sink.abortReasons) == 0 {
		t.Fatal("expected sink.Abort to be called on chunk fault")
	}
}

func TestTransferChunkDecodeFailureAborts(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, protoXferBegin{
		Type:     msgXferBegin,
		XferID:   "xfer-d1",
		Size:     uint32(len(payload)),
		Checksum: xxhashStr(payload),
	})
	_ = readMsg[protoXferReady](t, cm5)

	// Bogus base64 (uses non-base64url chars).
	sendMsg(t, cm5, protoXferChunk{
		Type:   msgXferChunk,
		XferID: "xfer-d1",
		Offset: 0,
		Data:   "!!!not-base64!!!",
	})

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Err != "decode_failed" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
	if len(sink.abortReasons) == 0 {
		t.Fatal("expected sink.Abort on decode failure")
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
	sendMsg(t, cm5, protoXferBegin{
		Type:     msgXferBegin,
		XferID:   "xfer-d2",
		Size:     uint32(len(payload)),
		Checksum: xxhashStr(payload),
	})
	_ = readMsg[protoXferReady](t, cm5)

	sendMsg(t, cm5, protoXferChunk{
		Type:   msgXferChunk,
		XferID: "xfer-d2",
		Offset: 0,
		Data:   rawURL([]byte("abcdef")),
	})

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Err != "size_overflow" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
}

func TestTransferCommitChecksumMismatchAborts(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	// Begin with the wrong-checksum advertised. The only way to surface a
	// commit-time mismatch is for begin/commit checksums to disagree, OR for
	// the streamed bytes to disagree with the begin checksum. Use the
	// latter: claim a bogus begin/commit checksum but stream the real bytes.
	bogus := strings.Repeat("0", 8)
	sendMsg(t, cm5, protoXferBegin{
		Type:     msgXferBegin,
		XferID:   "xfer-4",
		Size:     uint32(len(payload)),
		Checksum: bogus,
	})
	_ = readMsg[protoXferReady](t, cm5)

	sendMsg(t, cm5, protoXferChunk{
		Type:   msgXferChunk,
		XferID: "xfer-4",
		Offset: 0,
		Data:   rawURL(payload),
	})
	_ = readMsg[protoXferNeed](t, cm5)

	sendMsg(t, cm5, protoXferCommit{
		Type:     msgXferCommit,
		XferID:   "xfer-4",
		Size:     uint32(len(payload)),
		Checksum: bogus,
	})

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Type != msgXferAbort || abort.Err != "checksum_mismatch" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
	if len(sink.abortReasons) == 0 {
		t.Fatal("expected sink abort on checksum mismatch")
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
		nodeID:   "mcu-1",
		peerID:   "cm5-local",
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
	sendMsg(t, cm5, protoXferBegin{
		Type:     msgXferBegin,
		XferID:   "xfer-wd",
		Size:     uint32(len(payload)),
		Checksum: xxhashStr(payload),
	})
	_ = readMsg[protoXferReady](t, cm5)

	// Stop sending chunks; watchdog should fire within ~PhaseTimeout +
	// one exportTickInterval (50ms).
	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Type != msgXferAbort || abort.XferID != "xfer-wd" || abort.Err != "timeout" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
	if len(sink.abortReasons) == 0 || sink.abortReasons[0] != "timeout" {
		t.Fatalf("sink.Abort reasons = %v, want [\"timeout\"]", sink.abortReasons)
	}
}

func TestTransferCommitChecksumMismatchOnCommitFrameAborts(t *testing.T) {
	// xfer_begin and xfer_commit must agree on the checksum. If they
	// disagree (even when the streamed bytes match begin), commit aborts.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	good := xxhashStr(payload)
	sendMsg(t, cm5, protoXferBegin{
		Type:     msgXferBegin,
		XferID:   "xfer-5",
		Size:     uint32(len(payload)),
		Checksum: good,
	})
	_ = readMsg[protoXferReady](t, cm5)

	sendMsg(t, cm5, protoXferChunk{
		Type:   msgXferChunk,
		XferID: "xfer-5",
		Offset: 0,
		Data:   rawURL(payload),
	})
	_ = readMsg[protoXferNeed](t, cm5)

	// Commit advertises a different checksum than begin: must abort.
	sendMsg(t, cm5, protoXferCommit{
		Type:     msgXferCommit,
		XferID:   "xfer-5",
		Size:     uint32(len(payload)),
		Checksum: strings.Repeat("0", 8),
	})

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Type != msgXferAbort || abort.Err != "checksum_mismatch" {
		t.Fatalf("bad xfer_abort: %+v", abort)
	}
}

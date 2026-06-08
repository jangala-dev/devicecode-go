package fabric

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/otadiag"
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

type diagCapture struct {
	mu    sync.Mutex
	lines []string
}

func captureOTADiag(t *testing.T) *diagCapture {
	t.Helper()
	c := &diagCapture{}
	restore := otadiag.SetSinkForTest(func(line string) {
		c.mu.Lock()
		c.lines = append(c.lines, line)
		c.mu.Unlock()
	})
	t.Cleanup(restore)
	return c
}

func (c *diagCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func assertDiagContains(t *testing.T, lines []string, want ...string) {
	t.Helper()
	for _, line := range lines {
		matched := true
		for _, part := range want {
			if !strings.Contains(line, part) {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("diagnostics missing %v in:\n%s", want, strings.Join(lines, "\n"))
}

func waitDiagContains(t *testing.T, c *diagCapture, want ...string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		lines := c.snapshot()
		for _, line := range lines {
			matched := true
			for _, part := range want {
				if !strings.Contains(line, part) {
					matched = false
					break
				}
			}
			if matched {
				return
			}
		}
		if time.Now().After(deadline) {
			assertDiagContains(t, lines, want...)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertDiagNotContains(t *testing.T, lines []string, want ...string) {
	t.Helper()
	for _, line := range lines {
		matched := true
		for _, part := range want {
			if !strings.Contains(line, part) {
				matched = false
				break
			}
		}
		if matched {
			t.Fatalf("diagnostics unexpectedly contained %v in:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

func assertDiagOrder(t *testing.T, lines []string, wants ...[]string) {
	t.Helper()
	next := 0
	for _, line := range lines {
		if next >= len(wants) {
			return
		}
		matched := true
		for _, part := range wants[next] {
			if !strings.Contains(line, part) {
				matched = false
				break
			}
		}
		if matched {
			next++
		}
	}
	if next < len(wants) {
		t.Fatalf("diagnostics missing ordered item %d %v in:\n%s", next, wants[next], strings.Join(lines, "\n"))
	}
}

func waitDiagOrder(t *testing.T, c *diagCapture, wants ...[]string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		lines := c.snapshot()
		next := 0
		for _, line := range lines {
			if next >= len(wants) {
				return
			}
			matched := true
			for _, part := range wants[next] {
				if !strings.Contains(line, part) {
					matched = false
					break
				}
			}
			if matched {
				next++
			}
		}
		if next >= len(wants) {
			return
		}
		if time.Now().After(deadline) {
			assertDiagOrder(t, lines, wants...)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type blockingVerifier struct {
	entered  chan struct{}
	release  chan struct{}
	manifest updater.Manifest
}

func (v *blockingVerifier) Verify(r io.Reader, sink updater.SlotSink) (updater.Manifest, error) {
	select {
	case <-v.entered:
	default:
		close(v.entered)
	}
	<-v.release
	if _, err := io.Copy(sink, r); err != nil {
		return updater.Manifest{}, err
	}
	if err := sink.Commit(); err != nil {
		return updater.Manifest{}, err
	}
	return v.manifest, nil
}

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

func runUpdaterForFabricTest(t *testing.T, b *bus.Bus, opts updater.Options) (context.CancelFunc, *updater.Service) {
	t.Helper()
	if opts.Conn == nil {
		opts.Conn = b.NewConnection("updater")
	}
	if opts.Identity.Version == "" {
		opts.Identity = updater.Identity{Version: "0.0.0-test", Build: "build-test", ImageID: "img-test"}
	}
	probeConn := b.NewConnection("updater-probe")
	probe := probeConn.Subscribe(updater.TopicSoftwareFact)
	defer probeConn.Unsubscribe(probe)
	svc := updater.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	go svc.Run(ctx)
	select {
	case msg := <-probe.Channel():
		if msg == nil {
			cancel()
			t.Fatal("nil initial updater software fact")
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timeout waiting for updater service start")
	}
	return cancel, svc
}

func prepareUpdaterForFabricTest(t *testing.T, conn *bus.Connection) {
	t.Helper()
	msg := conn.NewMessage(updater.TopicPrepareRPC, updater.PrepareRequest{Target: updater.PrepareTargetMCU}, false)
	sub := conn.Request(msg)
	defer conn.Unsubscribe(sub)
	select {
	case rep := <-sub.Channel():
		if rep == nil {
			t.Fatal("nil prepare reply")
		}
		reply, ok := rep.Payload.(updater.PrepareReply)
		if !ok || !reply.Ready {
			t.Fatalf("prepare reply = %#v, want ready", rep.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prepare reply")
	}
}

func waitUpdaterFactForFabricTest(t *testing.T, sub *bus.Subscription, want func(updater.UpdaterFact) bool) updater.UpdaterFact {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sub.Channel():
			if msg == nil {
				continue
			}
			fact, ok := msg.Payload.(updater.UpdaterFact)
			if ok && (want == nil || want(fact)) {
				return fact
			}
		case <-deadline:
			t.Fatal("timeout waiting for updater fact")
		}
	}
}

func requestUpdaterForFabricTest(t *testing.T, conn *bus.Connection, topic bus.Topic, payload any) any {
	t.Helper()
	msg := conn.NewMessage(topic, payload, false)
	sub := conn.Request(msg)
	defer conn.Unsubscribe(sub)
	select {
	case rep := <-sub.Channel():
		if rep == nil {
			t.Fatal("nil updater reply")
		}
		return rep.Payload
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for updater reply")
	}
	return nil
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

func readTransferNeed(t *testing.T, tr Transport, id string, next uint32) {
	t.Helper()
	need := readMsg[protoXferNeed](t, tr)
	if need.Type != msgXferNeed || need.XferID != id || need.Next != next {
		t.Fatalf("bad xfer_need: %+v, want id=%s next=%d", need, id, next)
	}
}

func readTransferAbort(t *testing.T, tr Transport, id, reason string) {
	t.Helper()
	abort := readMsg[protoXferAbort](t, tr)
	if abort.Type != msgXferAbort || abort.XferID != id || abort.Err != reason {
		t.Fatalf("bad xfer_abort: %+v, want id=%s err=%s", abort, id, reason)
	}
}

func writeRawLine(t *testing.T, tr Transport, line string) {
	t.Helper()
	if err := tr.WriteLine([]byte(line)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
}

func TestTransferBeginWithoutPrepareAbortsNoReady(t *testing.T) {
	diag := captureOTADiag(t)
	b := newBus()
	cancelUpdater, _ := runUpdaterForFabricTest(t, b, updater.Options{})
	defer cancelUpdater()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-no-prepare", payload, nil))
	abort := readMsg[protoXferAbort](t, cm5)
	if abort.Type != msgXferAbort || abort.XferID != "xfer-no-prepare" || abort.Err != "stage_not_prepared" {
		t.Fatalf("xfer_begin without prepare frame = %+v, want stage_not_prepared abort", abort)
	}
	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-no-prepare", "ev begin_transfer_error", "err stage_not_prepared", "abort_tx true")
	waitDiagContains(t, diag, "[updater-stream]", "xfer_id xfer-no-prepare", "ev lease_error", "err stage_not_prepared")
}

func TestPreparedTransferBeginSendsReadyThenNeedZero(t *testing.T) {
	diag := captureOTADiag(t)
	b := newBus()
	cancelUpdater, _ := runUpdaterForFabricTest(t, b, updater.Options{})
	defer cancelUpdater()
	caller := b.NewConnection("caller")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(updater.TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	prepareUpdaterForFabricTest(t, caller)

	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-prepared", payload, nil))
	readTransferReady(t, cm5, "xfer-prepared", 0)
	waitUpdaterFactForFabricTest(t, upSub, func(f updater.UpdaterFact) bool {
		return f.State == updater.StateReceiving
	})
	waitDiagOrder(t, diag,
		[]string{"[fabric-xfer]", "xfer_id xfer-prepared", "ev begin_route_start"},
		[]string{"[fabric-xfer]", "xfer_id xfer-prepared", "ev begin_rx"},
		[]string{"[fabric-xfer]", "xfer_id xfer-prepared", "ev begin_validate_ok", "target updater/main"},
		[]string{"[fabric-xfer]", "xfer_id xfer-prepared", "ev begin_transfer_start"},
		[]string{"[updater-stream]", "xfer_id xfer-prepared", "ev begin_entry"},
		[]string{"[updater-stream]", "xfer_id xfer-prepared", "ev begin_exit"},
		[]string{"[fabric-xfer]", "xfer_id xfer-prepared", "ev begin_transfer_done"},
		[]string{"[fabric-xfer]", "xfer_id xfer-prepared", "ev ready_tx", "ok true"},
		[]string{"[fabric-xfer]", "xfer_id xfer-prepared", "ev need_tx", "next 0", "ok true"},
	)
}

func TestInvalidTransferBeginEmitsRejectDiagnosticNoActiveTransfer(t *testing.T) {
	diag := captureOTADiag(t)
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	beginCount := 0
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		beginTransfer: func(meta transferMeta) (transferSink, error) {
			beginCount++
			return &fakeTransferSink{}, nil
		},
	}
	go s.run(ctx)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, protoXferBegin{
		Type:      msgXferBegin,
		XferID:    "xfer-invalid",
		Target:    "other/target",
		Size:      uint32(len(payload)),
		DigestAlg: updater.DigestAlgXXHash32,
		Digest:    xxhashStr(payload),
	})
	readTransferAbort(t, cm5, "xfer-invalid", "bad_message: unsupported_target")
	if beginCount != 0 {
		t.Fatalf("beginTransfer called %d times for invalid begin, want 0", beginCount)
	}
	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-invalid", "ev begin_reject", "reason bad_message:unsupported_target", "abort_tx true")
}

func TestTransferAbortCancelsUpdaterLease(t *testing.T) {
	b := newBus()
	cancelUpdater, _ := runUpdaterForFabricTest(t, b, updater.Options{})
	defer cancelUpdater()
	caller := b.NewConnection("caller")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(updater.TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	prepareUpdaterForFabricTest(t, caller)

	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-abort-cancel", payload, nil))
	readTransferReady(t, cm5, "xfer-abort-cancel", 0)
	sendMsg(t, cm5, protoXferAbort{Type: msgXferAbort, XferID: "xfer-abort-cancel", Err: "host_abort"})

	fact := waitUpdaterFactForFabricTest(t, upSub, func(f updater.UpdaterFact) bool {
		return f.State == updater.StateFailed
	})
	if fact.LastError == nil || *fact.LastError != "host_abort" {
		t.Fatalf("updater last_error = %v, want host_abort", fact.LastError)
	}
}

func TestTransferTargetRejectCancelsLeaseAndPreventsCommit(t *testing.T) {
	b := newBus()
	memMD := updater.NewMemoryMetadata()
	cancelUpdater, _ := runUpdaterForFabricTest(t, b, updater.Options{
		Verifier:      updater.StubVerifier(),
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancelUpdater()
	caller := b.NewConnection("caller")
	prepareUpdaterForFabricTest(t, caller)

	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-target-reject", payload, nil))
	readTransferReady(t, cm5, "xfer-target-reject", 0)
	sendMsg(t, cm5, xferChunk("xfer-target-reject", 0, payload))
	_ = readMsg[protoXferNeed](t, cm5)
	sendMsg(t, cm5, xferCommit("xfer-target-reject", payload))

	abort := readMsg[protoXferAbort](t, cm5)
	if abort.XferID != "xfer-target-reject" || !strings.Contains(abort.Err, "verifier_stub") {
		t.Fatalf("xfer_abort = %+v, want verifier_stub rejection", abort)
	}
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("stage rejection left a staged descriptor")
	}

	replyPayload := requestUpdaterForFabricTest(t, caller, updater.TopicCommitRPC, updater.CommitRequest{})
	reply, ok := replyPayload.(updater.Reply)
	if !ok || reply.OK || reply.Error != updater.ErrNothingStaged {
		t.Fatalf("commit after rejected transfer = %#v, want nothing_staged", replyPayload)
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

func TestTransferAcceptedChunkEmitsProcessingDiagnostics(t *testing.T) {
	diag := captureOTADiag(t)
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-chunk-diag", payload, nil))
	readTransferReady(t, cm5, "xfer-chunk-diag", 0)

	sendMsg(t, cm5, xferChunk("xfer-chunk-diag", 0, payload))
	need := readMsg[protoXferNeed](t, cm5)
	if need.Next != uint32(len(payload)) {
		t.Fatalf("xfer_need.next = %d, want %d", need.Next, len(payload))
	}
	waitDiagOrder(t, diag,
		[]string{"[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev chunk_rx", "offset 0", "expected 0"},
		[]string{"[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev chunk_decode_done", "ok true", "raw_len 4"},
		[]string{"[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev chunk_digest_done", "ok true"},
		[]string{"[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev sink_write_start", "offset 0", "raw_len 4"},
		[]string{"[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev sink_write_done", "next 4"},
		[]string{"[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev gc_start", "next 4"},
		[]string{"[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev gc_done", "next 4"},
		[]string{"[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev need_tx", "next 4", "ok true", "accepted true"},
	)
	assertDiagNotContains(t, diag.snapshot(), "[fabric-xfer]", "xfer_id xfer-chunk-diag", "ev transfer_mem_sample")
}

func TestTransferAcceptedChunkEmitsSparseMemorySample(t *testing.T) {
	diag := captureOTADiag(t)
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := make([]byte, transferMemSampleStride)
	for i := range payload {
		payload[i] = byte(i)
	}
	sendMsg(t, cm5, xferBegin("xfer-mem-diag", payload, nil))
	readTransferReady(t, cm5, "xfer-mem-diag", 0)

	const chunkSize = 2048
	for off := 0; off < len(payload); off += chunkSize {
		end := off + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		sendMsg(t, cm5, xferChunk("xfer-mem-diag", uint32(off), payload[off:end]))
		need := readMsg[protoXferNeed](t, cm5)
		if need.Next != uint32(end) {
			t.Fatalf("xfer_need.next = %d, want %d", need.Next, end)
		}
	}
	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-mem-diag", "ev transfer_mem_sample", "next 65536", "alloc", "heap")
}

func TestTransferChunkFutureOffsetRequestsCurrentAndCompletes(t *testing.T) {
	diag := captureOTADiag(t)
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
	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-future-offset", "ev chunk_future_offset", "offset 7", "expected 0")

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
	diag := captureOTADiag(t)
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
	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-stale-offset", "ev chunk_stale_offset", "offset 0", "expected 3")

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

func TestTransferStaleLowerOffsetDoesNotRefreshPhaseDeadline(t *testing.T) {
	tr := &captureTransport{}
	sink := &fakeTransferSink{}
	oldDeadline := time.Now().Add(-time.Second)
	s := &session{
		tr:  tr,
		cfg: LinkConfig{PhaseTimeout: time.Second},
		incomingTransfer: &incomingTransfer{
			meta:         transferMeta{ID: "xfer-stale-deadline", Size: 6},
			sink:         sink,
			bytesWritten: 3,
			deadline:     oldDeadline,
		},
	}

	s.onTransferChunk(&protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-stale-deadline",
		Offset:      0,
		Data:        rawURL([]byte("abc")),
		ChunkDigest: xxhashStr([]byte("abc")),
	})

	if !s.incomingTransfer.deadline.Equal(oldDeadline) {
		t.Fatalf("stale lower offset refreshed deadline: got %v want %v",
			s.incomingTransfer.deadline, oldDeadline)
	}
	if len(tr.writes) != 1 {
		t.Fatalf("stale lower offset wrote %d frames, want one xfer_need", len(tr.writes))
	}
	if len(sink.writes) != 0 {
		t.Fatalf("stale lower offset rewrote sink: %d writes", len(sink.writes))
	}
}

func TestTransferCurrentCorruptChunkRefreshesLinkLiveness(t *testing.T) {
	tr := &captureTransport{}
	oldRx := time.Now().Add(-time.Second)
	s := &session{
		tr:       tr,
		lastRxAt: oldRx,
		cfg:      LinkConfig{PhaseTimeout: time.Second},
		incomingTransfer: &incomingTransfer{
			meta:     transferMeta{ID: "xfer-corrupt-liveness", Size: 4},
			sink:     &fakeTransferSink{},
			deadline: time.Now().Add(time.Second),
		},
	}

	s.onTransferChunk(&protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-corrupt-liveness",
		Offset:      0,
		Data:        rawURL([]byte("abcd")),
		ChunkDigest: "00000000",
	})

	if !s.lastRxAt.After(oldRx) {
		t.Fatalf("current corrupt chunk did not refresh liveness: got %v old %v", s.lastRxAt, oldRx)
	}
	if got := s.incomingTransfer.corruptRetriesAtOffset; got != 1 {
		t.Fatalf("corrupt retries = %d, want 1", got)
	}
	if len(tr.writes) != 1 {
		t.Fatalf("current corrupt chunk wrote %d frames, want one retry xfer_need", len(tr.writes))
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

func TestTransferChunkMissingDigestRetriesThenAborts(t *testing.T) {
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

	for i := 0; i < transferCorruptRetryLimit; i++ {
		sendMsg(t, cm5, protoXferChunk{
			Type:   msgXferChunk,
			XferID: "xfer-missing-digest",
			Offset: 0,
			Data:   rawURL(payload),
		})
		readTransferNeed(t, cm5, "xfer-missing-digest", 0)
	}
	sendMsg(t, cm5, protoXferChunk{
		Type:   msgXferChunk,
		XferID: "xfer-missing-digest",
		Offset: 0,
		Data:   rawURL(payload),
	})
	readTransferAbort(t, cm5, "xfer-missing-digest", "bad_message")
	if len(sink.abortReasons) == 0 {
		t.Fatal("expected sink.Abort on missing chunk digest")
	}
}

func TestTransferChunkInvalidBase64RetriesThenAborts(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-bad-b64", payload, nil))
	readTransferReady(t, cm5, "xfer-bad-b64", 0)

	for i := 0; i < transferCorruptRetryLimit; i++ {
		sendMsg(t, cm5, protoXferChunk{
			Type:        msgXferChunk,
			XferID:      "xfer-bad-b64",
			Offset:      0,
			Data:        "!!!not-base64!!!",
			ChunkDigest: xxhashStr(payload),
		})
		readTransferNeed(t, cm5, "xfer-bad-b64", 0)
	}
	sendMsg(t, cm5, protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-bad-b64",
		Offset:      0,
		Data:        "!!!not-base64!!!",
		ChunkDigest: xxhashStr(payload),
	})
	readTransferAbort(t, cm5, "xfer-bad-b64", "invalid_chunk_encoding")
	if len(sink.abortReasons) == 0 || sink.abortReasons[0] != "invalid_chunk_encoding" {
		t.Fatalf("sink.Abort reasons = %v, want invalid_chunk_encoding", sink.abortReasons)
	}
}

func TestTransferChunkDigestMismatchRequestsSameOffset(t *testing.T) {
	diag := captureOTADiag(t)
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
	lines := diag.snapshot()
	assertDiagContains(t, lines, "[fabric-xfer]", "xfer_id xfer-bad-chunk-digest", "ev chunk_digest_done", "ok false", "reason chunk_digest_mismatch")
	assertDiagNotContains(t, lines, "[fabric-xfer]", "xfer_id xfer-bad-chunk-digest", "ev sink_write_start")
	assertDiagNotContains(t, lines, "[fabric-xfer]", "xfer_id xfer-bad-chunk-digest", "ev gc_start")

	sendMsg(t, cm5, xferChunk("xfer-bad-chunk-digest", 0, payload))
	need = readMsg[protoXferNeed](t, cm5)
	if need.Next != uint32(len(payload)) {
		t.Fatalf("xfer_need.next after retry = %d, want %d", need.Next, len(payload))
	}
}

func TestTransferChunkWriteErrorEmitsAbortDiagnostic(t *testing.T) {
	diag := captureOTADiag(t)
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{writeErr: errors.New("write_boom")}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-write-error", payload, nil))
	readTransferReady(t, cm5, "xfer-write-error", 0)

	sendMsg(t, cm5, xferChunk("xfer-write-error", 0, payload))
	readTransferAbort(t, cm5, "xfer-write-error", "write_boom")

	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-write-error", "ev sink_write_error", "reason write_boom")
	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-write-error", "ev abort_tx", "reason write_boom", "ok true")
}

func TestTransferChunkDigestMismatchRetriesThenAborts(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-bad-digest-budget", payload, nil))
	readTransferReady(t, cm5, "xfer-bad-digest-budget", 0)

	for i := 0; i < transferCorruptRetryLimit; i++ {
		sendMsg(t, cm5, protoXferChunk{
			Type:        msgXferChunk,
			XferID:      "xfer-bad-digest-budget",
			Offset:      0,
			Data:        rawURL(payload),
			ChunkDigest: "00000000",
		})
		readTransferNeed(t, cm5, "xfer-bad-digest-budget", 0)
	}
	sendMsg(t, cm5, protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-bad-digest-budget",
		Offset:      0,
		Data:        rawURL(payload),
		ChunkDigest: "00000000",
	})
	readTransferAbort(t, cm5, "xfer-bad-digest-budget", "chunk_digest_mismatch")
}

func TestTransferMalformedCurrentChunkJSONRetriesThenAborts(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	id := "xfer-malformed-json"
	sendMsg(t, cm5, xferBegin(id, payload, nil))
	readTransferReady(t, cm5, id, 0)

	line := `{"type":"xfer_chunk","xfer_id":"` + id + `","offset":0,"data":"` + rawURL(payload) + `","chunk_digest":"` + xxhashStr(payload) + `","extra":true}`
	for i := 0; i < transferCorruptRetryLimit; i++ {
		writeRawLine(t, cm5, line)
		readTransferNeed(t, cm5, id, 0)
	}
	writeRawLine(t, cm5, line)
	readTransferAbort(t, cm5, id, "bad_message")
}

func TestTransferMalformedWrongXferIDDoesNotChargeActiveTransfer(t *testing.T) {
	payload := []byte("abcd")
	activeID := "xfer-active-malformed"
	cases := []struct {
		name string
		line string
	}{
		{
			name: "wrong_id",
			line: `{"type":"xfer_chunk","xfer_id":"xfer-other","offset":0,"data":"` + rawURL(payload) + `","chunk_digest":"` + xxhashStr(payload) + `","extra":true}`,
		},
		{
			name: "missing_id",
			line: `{"type":"xfer_chunk","offset":0,"data":"` + rawURL(payload) + `","chunk_digest":"` + xxhashStr(payload) + `","extra":true}`,
		},
		{
			name: "unreadable_id",
			line: `{"type":"xfer_chunk","xfer_id":{"bad":true},"offset":0,"data":"` + rawURL(payload) + `","chunk_digest":"` + xxhashStr(payload) + `","extra":true}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeTransferSink{}
			tr := &captureTransport{}
			s := &session{
				link: linkUp,
				tr:   tr,
				incomingTransfer: &incomingTransfer{
					meta:     transferMeta{ID: activeID},
					sink:     sink,
					deadline: time.Now().Add(time.Second),
				},
			}

			for i := 0; i < transferCorruptRetryLimit+1; i++ {
				s.dispatch([]byte(tc.line))
			}
			if len(tr.writes) != 0 {
				t.Fatalf("malformed non-current xfer_chunk emitted %d frames, want none", len(tr.writes))
			}
			if s.incomingTransfer == nil {
				t.Fatal("malformed non-current xfer_chunk cleared active transfer")
			}
			if got := s.incomingTransfer.corruptRetriesAtOffset; got != 0 {
				t.Fatalf("corrupt retries at offset = %d, want 0", got)
			}
			if len(sink.abortReasons) != 0 {
				t.Fatalf("sink aborted for non-current malformed chunk: %v", sink.abortReasons)
			}
		})
	}
}

func TestTransferCorruptRetryBudgetResetsAfterAcceptedProgress(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	id := "xfer-retry-reset"
	payload := []byte("abcdef")
	first := []byte("abc")
	second := []byte("def")
	sendMsg(t, cm5, xferBegin(id, payload, nil))
	readTransferReady(t, cm5, id, 0)

	for i := 0; i < transferCorruptRetryLimit-1; i++ {
		sendMsg(t, cm5, protoXferChunk{
			Type:        msgXferChunk,
			XferID:      id,
			Offset:      0,
			Data:        rawURL(first),
			ChunkDigest: "00000000",
		})
		readTransferNeed(t, cm5, id, 0)
	}
	sendMsg(t, cm5, xferChunk(id, 0, first))
	readTransferNeed(t, cm5, id, uint32(len(first)))

	for i := 0; i < transferCorruptRetryLimit; i++ {
		sendMsg(t, cm5, protoXferChunk{
			Type:        msgXferChunk,
			XferID:      id,
			Offset:      uint32(len(first)),
			Data:        rawURL(second),
			ChunkDigest: "00000000",
		})
		readTransferNeed(t, cm5, id, uint32(len(first)))
	}
	sendMsg(t, cm5, xferChunk(id, uint32(len(first)), second))
	readTransferNeed(t, cm5, id, uint32(len(payload)))
	sendMsg(t, cm5, xferCommit(id, payload))
	done := readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != id {
		t.Fatalf("bad xfer_done: %+v", done)
	}
}

func TestTransferFutureOffsetDoesNotResetCorruptRetryBudget(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	id := "xfer-future-no-reset"
	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin(id, payload, nil))
	readTransferReady(t, cm5, id, 0)

	for i := 0; i < transferCorruptRetryLimit; i++ {
		sendMsg(t, cm5, protoXferChunk{
			Type:        msgXferChunk,
			XferID:      id,
			Offset:      0,
			Data:        rawURL(payload),
			ChunkDigest: "00000000",
		})
		readTransferNeed(t, cm5, id, 0)
	}
	sendMsg(t, cm5, xferChunk(id, 99, payload))
	readTransferNeed(t, cm5, id, 0)
	sendMsg(t, cm5, protoXferChunk{
		Type:        msgXferChunk,
		XferID:      id,
		Offset:      0,
		Data:        rawURL(payload),
		ChunkDigest: "00000000",
	})
	readTransferAbort(t, cm5, id, "chunk_digest_mismatch")
}

func TestTransferStaleOffsetDoesNotResetCorruptRetryBudget(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{}
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	id := "xfer-stale-no-reset"
	payload := []byte("abcdef")
	first := []byte("abc")
	second := []byte("def")
	sendMsg(t, cm5, xferBegin(id, payload, nil))
	readTransferReady(t, cm5, id, 0)
	sendMsg(t, cm5, xferChunk(id, 0, first))
	readTransferNeed(t, cm5, id, uint32(len(first)))

	for i := 0; i < transferCorruptRetryLimit; i++ {
		sendMsg(t, cm5, protoXferChunk{
			Type:        msgXferChunk,
			XferID:      id,
			Offset:      uint32(len(first)),
			Data:        rawURL(second),
			ChunkDigest: "00000000",
		})
		readTransferNeed(t, cm5, id, uint32(len(first)))
	}
	sendMsg(t, cm5, xferChunk(id, 0, first))
	readTransferNeed(t, cm5, id, uint32(len(first)))
	sendMsg(t, cm5, protoXferChunk{
		Type:        msgXferChunk,
		XferID:      id,
		Offset:      uint32(len(first)),
		Data:        rawURL(second),
		ChunkDigest: "00000000",
	})
	readTransferAbort(t, cm5, id, "chunk_digest_mismatch")
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

func TestTransferTargetInvokedAfterCommit(t *testing.T) {
	// With target=updater/main, fabric calls the local updater stage RPC
	// after xfer_commit and before xfer_done. The wire never names a
	// raw/member receiver topic.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gotPayload := installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})

	sink := &fakeTransferSink{commitInfo: transferInfo{BytesWritten: 4, Generation: 7}}
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		beginTransfer: func(meta transferMeta) (transferSink, error) {
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
		if p.Size != uint32(len(payload)) || p.Generation != 7 {
			t.Fatalf("stage size/generation wrong: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stage call")
	}

	done := readMsg[protoXferDone](t, cm5)
	if done.XferID != "xfer-stage" {
		t.Fatalf("xfer_done xfer_id = %q, want xfer-stage", done.XferID)
	}
}

func TestCompletedTransferDuplicateBeginSameTupleReplaysDone(t *testing.T) {
	diag := captureOTADiag(t)
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stageCalls := installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})
	sink := &fakeTransferSink{}
	beginCount := 0
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		beginTransfer: func(meta transferMeta) (transferSink, error) {
			beginCount++
			return sink, nil
		},
	}
	go s.run(ctx)
	bringUp(t, cm5)

	id := "xfer-completed-replay"
	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin(id, payload, nil))
	readTransferReady(t, cm5, id, 0)
	sendMsg(t, cm5, xferChunk(id, 0, payload))
	readTransferNeed(t, cm5, id, uint32(len(payload)))
	sendMsg(t, cm5, xferCommit(id, payload))
	select {
	case p := <-stageCalls:
		if p.XferID != id {
			t.Fatalf("stage xfer_id = %q, want %q", p.XferID, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for first stage call")
	}
	done := readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != id {
		t.Fatalf("bad xfer_done: %+v", done)
	}
	if beginCount != 1 {
		t.Fatalf("beginTransfer calls after first completion = %d, want 1", beginCount)
	}

	sendMsg(t, cm5, xferBegin(id, payload, nil))
	done = readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != id {
		t.Fatalf("duplicate begin response = %+v, want xfer_done", done)
	}
	if beginCount != 1 {
		t.Fatalf("duplicate completed begin reopened sink: beginCount=%d", beginCount)
	}
	select {
	case p := <-stageCalls:
		t.Fatalf("duplicate completed begin restaged transfer: %+v", p)
	default:
	}
	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-completed-replay", "ev begin_duplicate_done", "done_tx true")
}

func TestCompletedTransferDuplicateBeginConflictingTupleAborts(t *testing.T) {
	diag := captureOTADiag(t)
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})
	sink := &fakeTransferSink{}
	beginCount := 0
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		beginTransfer: func(meta transferMeta) (transferSink, error) {
			beginCount++
			return sink, nil
		},
	}
	go s.run(ctx)
	bringUp(t, cm5)

	id := "xfer-completed-conflict"
	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin(id, payload, nil))
	readTransferReady(t, cm5, id, 0)
	sendMsg(t, cm5, xferChunk(id, 0, payload))
	readTransferNeed(t, cm5, id, uint32(len(payload)))
	sendMsg(t, cm5, xferCommit(id, payload))
	done := readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != id {
		t.Fatalf("bad xfer_done: %+v", done)
	}

	sendMsg(t, cm5, xferBegin(id, []byte("abcde"), nil))
	readTransferAbort(t, cm5, id, "conflicting_transfer")
	if beginCount != 1 {
		t.Fatalf("conflicting completed begin reopened sink: beginCount=%d", beginCount)
	}
	waitDiagContains(t, diag, "[fabric-xfer]", "xfer_id xfer-completed-conflict", "ev begin_reject", "reason conflicting_transfer", "abort_tx true")
}

func TestTransferTargetRejectAbortsTransfer(t *testing.T) {
	// updater/main stage replies {ok=false, err=...}. fabric must send
	// xfer_abort with the stage reason rather than xfer_done.
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = installStageResponder(t, b, updater.StageReply{OK: false, Err: "manifest_check_failed"})

	sink := &fakeTransferSink{commitInfo: transferInfo{BytesWritten: 4, Generation: 7}}
	s := session{
		linkID:   defaultLinkID,
		nodeID:   "mcu",
		peerID:   "bigbox-cm5",
		localSID: "mcu-sid-test",
		tr:       mcu,
		conn:     b.NewConnection("fabric"),
		beginTransfer: func(meta transferMeta) (transferSink, error) {
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

func TestTransferTargetStageTimeoutCancelsLeaseAndPreventsLateStagePersist(t *testing.T) {
	b := newBus()
	memMD := updater.NewMemoryMetadata()
	verif := &blockingVerifier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		manifest: updater.Manifest{
			Version:       "9.9.9",
			BuildID:       "build-9.9.9",
			ImageID:       "mcu-dev-9.9.9",
			PayloadSHA256: strings.Repeat("a", 64),
			PayloadLength: 4,
		},
	}
	cancelUpdater, _ := runUpdaterForFabricTest(t, b, updater.Options{
		Verifier:      verif,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancelUpdater()
	caller := b.NewConnection("caller")
	prepareUpdaterForFabricTest(t, caller)

	cfg := DefaultLinkConfig()
	cfg.TargetCallTimeout = 20 * time.Millisecond

	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", cfg)
	bringUp(t, cm5)

	id := "xfer-stage-timeout"
	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin(id, payload, nil))
	readTransferReady(t, cm5, id, 0)
	sendMsg(t, cm5, xferChunk(id, 0, payload))
	readTransferNeed(t, cm5, id, uint32(len(payload)))
	sendMsg(t, cm5, xferCommit(id, payload))
	select {
	case <-verif.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("verifier did not start before stage timeout")
	}
	readTransferAbort(t, cm5, id, "stage_timeout")
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("stage timeout persisted descriptor before verifier returned")
	}

	close(verif.release)
	time.Sleep(50 * time.Millisecond)
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("late verifier completion after stage timeout persisted descriptor")
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

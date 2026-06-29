package fabric

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/updater"
	"devicecode-go/x/xxhash"
)

type fakeTransferSink struct {
	offs                []uint32
	writes              [][]byte
	writeErr            error
	writeEntered        chan struct{}
	writeEnterOnce      sync.Once
	writeRelease        chan struct{}
	commitErr           error
	commitEntered       chan struct{}
	commitEnterOnce     sync.Once
	commitRelease       chan struct{}
	applyErr            error
	commitInfo          transferInfo
	mutateDataAfterCopy bool
	committed           bool
	applied             bool
	abortReasons        []string
}

func (s *fakeTransferSink) WriteChunk(off uint32, data []byte) error {
	if s.writeEntered != nil {
		s.writeEnterOnce.Do(func() { close(s.writeEntered) })
	}
	if s.writeRelease != nil {
		<-s.writeRelease
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	s.offs = append(s.offs, off)
	s.writes = append(s.writes, append([]byte(nil), data...))
	if s.mutateDataAfterCopy && len(data) > 0 {
		data[0] ^= 0xff
	}
	return nil
}

func (s *fakeTransferSink) Commit() (transferInfo, error) {
	if s.commitEntered != nil {
		s.commitEnterOnce.Do(func() { close(s.commitEntered) })
	}
	if s.commitRelease != nil {
		<-s.commitRelease
	}
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

func waitAbortReason(t *testing.T, sink *fakeTransferSink, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if len(sink.abortReasons) > 0 {
			if want != "" && sink.abortReasons[0] != want {
				t.Fatalf("sink.Abort reasons = %v, want %q", sink.abortReasons, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for sink.Abort(%q); reasons=%v", want, sink.abortReasons)
		}
		time.Sleep(time.Millisecond)
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
	got := make(chan updater.StagePayload, 4)
	binding := conn.Bind(updater.TopicStageRPC, func(ctx context.Context, payload any) (any, error) {
		if p, ok := payload.(updater.StagePayload); ok {
			select {
			case got <- p:
			default:
			}
		}
		return reply, nil
	})
	t.Cleanup(binding.Close)
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
	case <-probe.Channel():
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timeout waiting for updater service start")
	}
	return cancel, svc
}

func prepareUpdaterForFabricTest(t *testing.T, conn *bus.Connection) {
	t.Helper()
	rep, err := conn.Call(context.Background(), updater.TopicPrepareRPC, updater.PrepareRequest{Target: updater.PrepareTargetMCU})
	if err != nil {
		t.Fatalf("prepare call failed: %v", err)
	}
	reply, ok := rep.(updater.PrepareReply)
	if !ok || !reply.Ready {
		t.Fatalf("prepare reply = %#v, want ready", rep)
	}
}

func waitUpdaterFactForFabricTest(t *testing.T, sub *bus.Subscription, want func(updater.UpdaterFact) bool) updater.UpdaterFact {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sub.Channel():
			if false {
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
	rep, err := conn.Call(context.Background(), topic, payload)
	if err != nil {
		t.Fatalf("updater call failed: %v", err)
	}
	return rep
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

func readUntilTransferAbort(t *testing.T, tr Transport, id, reason string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for xfer_abort id=%s err=%s", id, reason)
		}
		lineCh := make(chan []byte, 1)
		errCh := make(chan error, 1)
		go func() {
			line, err := tr.ReadLine()
			if err != nil {
				errCh <- err
				return
			}
			lineCh <- append([]byte(nil), line...)
		}()
		var line []byte
		select {
		case line = <-lineCh:
		case err := <-errCh:
			t.Fatalf("ReadLine: %v", err)
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timed out waiting for xfer_abort id=%s err=%s", id, reason)
		}
		var probe struct {
			Type   string `json:"type"`
			XferID string `json:"xfer_id"`
			Err    string `json:"err"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Fatalf("Unmarshal %q: %v", line, err)
		}
		if probe.Type != msgXferAbort {
			continue
		}
		if probe.XferID != id || probe.Err != reason {
			t.Fatalf("bad xfer_abort: %+v, want id=%s err=%s", probe, id, reason)
		}
		return
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

func TestTransferBeginWithoutStageControllerAbortsNoReady(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig())
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-no-controller", payload, nil))
	readTransferAbort(t, cm5, "xfer-no-controller", "updater_stage_controller_missing")
}

func TestTransferAbortCancelsUpdaterLease(t *testing.T) {
	b := newBus()
	cancelUpdater, updaterSvc := runUpdaterForFabricTest(t, b, updater.Options{})
	defer cancelUpdater()
	caller := b.NewConnection("caller")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(updater.TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	prepareUpdaterForFabricTest(t, caller)

	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunWithOptions(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig(), RunOptions{StageController: updaterSvc})
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
	cancelUpdater, updaterSvc := runUpdaterForFabricTest(t, b, updater.Options{
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
	go RunWithOptions(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig(), RunOptions{StageController: updaterSvc})
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
	if !ok || reply.OK || reply.Error != updater.ErrNoStagedImage {
		t.Fatalf("commit after rejected transfer = %#v, want no_staged_image", replyPayload)
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

func TestTransferChunkWriteIsSynchronousBeforeNeed(t *testing.T) {
	tr := &captureTransport{}
	writeEntered := make(chan struct{})
	writeRelease := make(chan struct{})
	sink := &fakeTransferSink{writeEntered: writeEntered, writeRelease: writeRelease}
	s := &session{
		tr:  tr,
		cfg: LinkConfig{PhaseTimeout: time.Second},
		incomingTransfer: &incomingTransfer{
			meta:     transferMeta{ID: "xfer-sync-write", Size: 8},
			sink:     sink,
			hasher:   xxhash.New(0),
			deadline: time.Now().Add(time.Second),
		},
	}
	done := make(chan struct{})
	go func() {
		s.onTransferChunk(&protoXferChunk{
			Type:        msgXferChunk,
			XferID:      "xfer-sync-write",
			Offset:      0,
			Data:        rawURL([]byte("abcd")),
			ChunkDigest: xxhashStr([]byte("abcd")),
		})
		close(done)
	}()

	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("WriteChunk was not entered")
	}
	if len(tr.writes) != 0 {
		t.Fatalf("xfer_need sent before WriteChunk returned: %d writes", len(tr.writes))
	}
	close(writeRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onTransferChunk did not return after WriteChunk release")
	}
	if len(tr.writes) != 1 {
		t.Fatalf("writes after accepted chunk = %d, want one xfer_need", len(tr.writes))
	}
	var need protoXferNeed
	if err := json.Unmarshal(tr.writes[0], &need); err != nil {
		t.Fatalf("decode xfer_need: %v", err)
	}
	if need.Type != msgXferNeed || need.XferID != "xfer-sync-write" || need.Next != 4 {
		t.Fatalf("xfer_need = %+v, want next=4", need)
	}
	if len(sink.writes) != 1 || string(sink.writes[0]) != "abcd" {
		t.Fatalf("sink writes = %q, want abcd", sink.writes)
	}
}

func TestTransferCommitReturnsBeforeAsyncStageResult(t *testing.T) {
	b := newBus()
	tr := &captureTransport{}
	commitEntered := make(chan struct{})
	commitRelease := make(chan struct{})
	sink := &fakeTransferSink{
		commitEntered: commitEntered,
		commitRelease: commitRelease,
		commitInfo:    transferInfo{BytesWritten: 4, Generation: 11},
	}
	gotStage := installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})
	payload := []byte("abcd")
	s := &session{
		linkID: defaultLinkID,
		tr:     tr,
		conn:   b.NewConnection("fabric"),
		cfg:    LinkConfig{TargetCallTimeout: time.Second},
		incomingTransfer: &incomingTransfer{
			meta:         transferMeta{ID: "xfer-sync-commit", Target: updater.TargetUpdaterMain, Size: 4, DigestAlg: updater.DigestAlgXXHash32, Digest: xxhashStr(payload)},
			sink:         sink,
			bytesWritten: 4,
			hasher:       xxhash.New(0),
			deadline:     time.Now().Add(time.Second),
		},
	}
	_, _ = s.incomingTransfer.hasher.Write(payload)
	done := make(chan struct{})
	go func() {
		s.onTransferCommit(&protoXferCommit{Type: msgXferCommit, XferID: "xfer-sync-commit", Size: 4, DigestAlg: updater.DigestAlgXXHash32, Digest: xxhashStr(payload)})
		close(done)
	}()

	select {
	case <-commitEntered:
	case <-time.After(time.Second):
		t.Fatal("Commit was not entered")
	}
	if s.pendingTargetCall != nil {
		t.Fatal("stage call started before Commit returned")
	}
	if s.incomingTransfer == nil {
		t.Fatal("active transfer cleared before Commit returned")
	}
	close(commitRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onTransferCommit did not return after Commit release")
	}
	if !sink.committed {
		t.Fatal("sink.Commit did not complete")
	}
	if s.incomingTransfer != nil {
		t.Fatal("incoming transfer not cleared after successful Commit")
	}
	if s.pendingTargetCall == nil {
		t.Fatal("pending target call missing before async stage result is handled")
	}
	select {
	case p := <-gotStage:
		if p.XferID != "xfer-sync-commit" {
			t.Fatalf("stage xfer_id = %q, want xfer-sync-commit", p.XferID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for async stage call")
	}
	select {
	case result := <-s.targetCallResults:
		s.handleTargetCallResult(result)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for async stage result")
	}
	if s.pendingTargetCall != nil {
		t.Fatalf("pending target call = %+v, want nil after async result", s.pendingTargetCall)
	}
}

func TestTransferCommitDigestUnaffectedBySinkMutatingWriteBuffer(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &fakeTransferSink{mutateDataAfterCopy: true, commitInfo: transferInfo{BytesWritten: 4, Generation: 7}}
	installStageResponder(t, b, updater.StageReply{OK: true, Stage: "staged"})
	go runSessionWithSink(ctx, mcu, b.NewConnection("fabric"), sink)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, xferBegin("xfer-mutating-sink", payload, nil))
	readTransferReady(t, cm5, "xfer-mutating-sink", 0)
	sendMsg(t, cm5, xferChunk("xfer-mutating-sink", 0, payload))
	readTransferNeed(t, cm5, "xfer-mutating-sink", uint32(len(payload)))
	sendMsg(t, cm5, xferCommit("xfer-mutating-sink", payload))
	done := readMsg[protoXferDone](t, cm5)
	if done.Type != msgXferDone || done.XferID != "xfer-mutating-sink" {
		t.Fatalf("bad xfer_done after mutating sink: %+v", done)
	}
	if len(sink.writes) != 1 || string(sink.writes[0]) != string(payload) {
		t.Fatalf("sink wrote %q, want %q", sink.writes, payload)
	}
	if len(sink.abortReasons) != 0 {
		t.Fatalf("sink aborted after mutating write buffer: %v", sink.abortReasons)
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
	if got := s.incomingTransfer.retriesAtOffset; got != 1 {
		t.Fatalf("corrupt retries = %d, want 1", got)
	}
	if len(tr.writes) != 1 {
		t.Fatalf("current corrupt chunk wrote %d frames, want one retry xfer_need", len(tr.writes))
	}
}

func TestTransferCorruptChunkDoesNotWriteOrAdvance(t *testing.T) {
	tr := &captureTransport{}
	sink := &fakeTransferSink{}
	s := &session{
		tr:  tr,
		cfg: LinkConfig{PhaseTimeout: time.Second},
		incomingTransfer: &incomingTransfer{
			meta:     transferMeta{ID: "xfer-corrupt-no-advance", Size: 4},
			sink:     sink,
			hasher:   xxhash.New(0),
			deadline: time.Now().Add(time.Second),
		},
	}

	s.onTransferChunk(&protoXferChunk{
		Type:        msgXferChunk,
		XferID:      "xfer-corrupt-no-advance",
		Offset:      0,
		Data:        rawURL([]byte("abcd")),
		ChunkDigest: "00000000",
	})

	if s.incomingTransfer == nil {
		t.Fatal("corrupt chunk cleared active transfer before retry budget was exhausted")
	}
	if s.incomingTransfer.bytesWritten != 0 {
		t.Fatalf("corrupt chunk advanced bytesWritten = %d, want 0", s.incomingTransfer.bytesWritten)
	}
	if len(sink.writes) != 0 {
		t.Fatalf("corrupt chunk called WriteChunk %d times, want 0", len(sink.writes))
	}
	if len(tr.writes) != 1 {
		t.Fatalf("corrupt chunk emitted %d frames, want one retry xfer_need", len(tr.writes))
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
	waitAbortReason(t, sink, "bad_message")
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
	waitAbortReason(t, sink, "invalid_chunk_encoding")
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
			if got := s.incomingTransfer.retriesAtOffset; got != 0 {
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
	waitAbortReason(t, sink, "digest_mismatch")
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

func TestTransferCommitTimeoutCancelsLeaseAndPreventsLateStagePersist(t *testing.T) {
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
	cancelUpdater, updaterSvc := runUpdaterForFabricTest(t, b, updater.Options{
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
	go RunWithOptions(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", cfg, RunOptions{StageController: updaterSvc})
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
		t.Fatal("verifier did not start before commit timeout")
	}
	// Let the configured commit deadline pass while the verifier/flash operation
	// remains blocked. The worker observes that deadline at the next safe point.
	time.Sleep(50 * time.Millisecond)
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("descriptor persisted while verifier was still blocked")
	}

	// The stage worker now owns the verifier/flash call directly rather than
	// spawning a nested goroutine.  The timeout is therefore observed at the
	// next safe point: after the bounded verifier operation returns.
	close(verif.release)
	readUntilTransferAbort(t, cm5, id, "transfer_commit_timeout")
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("late verifier completion after commit timeout persisted descriptor")
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
	waitAbortReason(t, sink, "timeout")
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

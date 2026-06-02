package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/otadiag"
	"pico2-a-b/imagev1"
)

// ---- helpers --------------------------------------------------------

func newTestBus() *bus.Bus { return bus.NewBus(8, "+", "#") }

type updaterDiagCapture struct {
	mu    sync.Mutex
	lines []string
}

func captureUpdaterDiag(t *testing.T) *updaterDiagCapture {
	t.Helper()
	c := &updaterDiagCapture{}
	restore := otadiag.SetSinkForTest(func(line string) {
		c.mu.Lock()
		c.lines = append(c.lines, line)
		c.mu.Unlock()
	})
	t.Cleanup(restore)
	return c
}

func (c *updaterDiagCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func assertUpdaterDiagContains(t *testing.T, lines []string, want ...string) {
	t.Helper()
	for _, line := range lines {
		ok := true
		for _, part := range want {
			if !strings.Contains(line, part) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("diagnostics missing %v in:\n%s", want, strings.Join(lines, "\n"))
}

type fakeVerifierAccept struct {
	manifest Manifest
	payload  []byte
}

func (f *fakeVerifierAccept) Verify(r io.Reader, sink SlotSink) (Manifest, error) {
	if f.payload != nil {
		_, _ = sink.Write(f.payload)
	} else {
		_, _ = io.Copy(sink, r)
	}
	if sink != nil {
		if err := sink.Commit(); err != nil {
			return Manifest{}, err
		}
	}
	return f.manifest, nil
}

type fakeVerifierReject struct{ err error }

func (f *fakeVerifierReject) Verify(r io.Reader, sink SlotSink) (Manifest, error) {
	_ = r
	if sink != nil {
		_ = sink.Abort()
	}
	return Manifest{}, f.err
}

type fakeMetadata struct {
	sha    string
	staged StagedDescriptor
	has    bool
}

func (f *fakeMetadata) PayloadSHA256() string                      { return f.sha }
func (f *fakeMetadata) StagedDescriptor() (StagedDescriptor, bool) { return f.staged, f.has }

type failingClearMetadata struct {
	*MemoryMetadata
	err error
}

func (f *failingClearMetadata) ClearStagedDescriptor() error {
	if f.err != nil {
		return f.err
	}
	return f.MemoryMetadata.ClearStagedDescriptor()
}

// fakeApplier always succeeds — used by tests that need the commit RPC
// to drive the state machine through committing/rebooting without
// actually rebooting (production wiring uses RefusingApplier so the
// commit RPC returns apply_unavailable until the real abupdate-backed
// implementation is supplied).
//
// canCalls and rebootCalls are kept separate so tests can verify the commit
// ordering: CanApply first, publish rebooting + reply accepted, then ArmReboot.
type fakeApplier struct {
	mu          sync.Mutex
	canCalls    []StagedDescriptor
	rebootCalls []StagedDescriptor
	rebootErr   error
	rebootCh    chan StagedDescriptor
}

func (f *fakeApplier) CanApply(d StagedDescriptor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canCalls = append(f.canCalls, d)
	return nil
}

func (f *fakeApplier) ArmReboot(d StagedDescriptor) error {
	f.mu.Lock()
	f.rebootCalls = append(f.rebootCalls, d)
	err := f.rebootErr
	ch := f.rebootCh
	f.mu.Unlock()
	if ch != nil {
		select {
		case ch <- d:
		default:
		}
	}
	return err
}

func (f *fakeApplier) callCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.canCalls), len(f.rebootCalls)
}

func (f *fakeApplier) rebootCall(i int) StagedDescriptor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rebootCalls[i]
}

// ---- boot_id --------------------------------------------------------

func TestBootIDIs16HexChars(t *testing.T) {
	resetBootIDForTest()
	id := GenerateBootID()
	if len(id) != 16 {
		t.Fatalf("len = %d, want 16", len(id))
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Fatalf("not hex: %v", err)
	}
}

func TestBootIDIsCachedAcrossCalls(t *testing.T) {
	// Within a process boot, GenerateBootID is idempotent — multiple
	// callers see the same value.
	resetBootIDForTest()
	a := GenerateBootID()
	b := GenerateBootID()
	if a != b {
		t.Fatalf("non-idempotent: %s vs %s", a, b)
	}
}

func TestBootIDChangesAfterReset(t *testing.T) {
	// resetBootIDForTest mimics a successful boot. 10 successive boots
	// must all produce unique values ("RNG-never-seeded / from-constant"
	// guard).
	seen := make(map[string]struct{})
	for i := 0; i < 10; i++ {
		resetBootIDForTest()
		id := GenerateBootID()
		if _, dup := seen[id]; dup {
			t.Fatalf("boot %d duplicated id %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestBootIDIsNotAllZero(t *testing.T) {
	// "Generated-before-entropy" guard: all-zero sentinel should never
	// be returned. The fallback path explicitly walks past it.
	for i := 0; i < 20; i++ {
		resetBootIDForTest()
		id := GenerateBootID()
		if id == "0000000000000000" {
			t.Fatal("got all-zero boot_id")
		}
	}
}

// ---- state machine + RPC handlers ----------------------------------

func waitForFact[T any](t *testing.T, sub *bus.Subscription, want func(T) bool) T {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sub.Channel():
			if msg == nil {
				continue
			}
			fact, ok := msg.Payload.(T)
			if !ok {
				continue
			}
			if want == nil || want(fact) {
				return fact
			}
		case <-deadline:
			t.Fatal("timeout waiting for fact")
		}
	}
}

func waitForCriticalFactSet(t *testing.T, swSub, upSub, hSub *bus.Subscription) (SoftwareFact, UpdaterFact, HealthFact) {
	t.Helper()
	sw := waitForFact[SoftwareFact](t, swSub, nil)
	up := waitForFact[UpdaterFact](t, upSub, nil)
	h := waitForFact[HealthFact](t, hSub, nil)
	return sw, up, h
}

func drainSubscription(sub *bus.Subscription) {
	for {
		select {
		case <-sub.Channel():
		default:
			return
		}
	}
}

func publishTestLinkState(conn *bus.Connection, ready bool, peerSID, localSID string) {
	conn.Publish(conn.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{
			"ready":       ready,
			"established": ready,
			"peer_sid":    peerSID,
			"local_sid":   localSID,
		},
		true,
	))
}

func testCriticalRepublishConfig() CriticalRepublishConfig {
	return CriticalRepublishConfig{
		BurstInterval:  20 * time.Millisecond,
		BurstDuration:  45 * time.Millisecond,
		SteadyInterval: 30 * time.Millisecond,
	}
}

func strValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func int32Value(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func testStagePayload(id string, artefact []byte) StagePayload {
	return StagePayload{
		LinkID:    "mcu-uart0",
		XferID:    id,
		Target:    TargetUpdaterMain,
		Size:      uint32(len(artefact)),
		DigestAlg: DigestAlgXXHash32,
		Digest:    "deadbeef",
		Artefact:  artefact,
	}
}

func preparedStagePayload(t *testing.T, caller *bus.Connection, svc *Service, id string, artefact []byte) StagePayload {
	t.Helper()
	req := caller.NewMessage(TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU}, false)
	sub := caller.Request(req)
	defer caller.Unsubscribe(sub)
	select {
	case msg := <-sub.Channel():
		if msg == nil {
			t.Fatal("nil prepare reply")
		}
		switch reply := msg.Payload.(type) {
		case PrepareReply:
			if !reply.Ready {
				t.Fatalf("prepare reply not ready: %+v", reply)
			}
		case Reply:
			t.Fatalf("prepare failed: %+v", reply)
		default:
			t.Fatalf("prepare payload type = %T", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prepare reply")
	}
	generation, err := svc.beginStreamedStageLease(id)
	if err != nil {
		t.Fatalf("begin stage lease: %v", err)
	}
	if err := svc.markStreamedStageCommitted(id, generation); err != nil {
		t.Fatalf("commit stage lease: %v", err)
	}
	payload := testStagePayload(id, artefact)
	payload.Generation = generation
	return payload
}

func runService(t *testing.T, b *bus.Bus, opts Options) (*Service, context.CancelFunc) {
	t.Helper()
	resetBootIDForTest()
	if opts.Conn == nil {
		t.Fatal("Options.Conn is required")
	}
	if opts.Identity.Version == "" {
		opts.Identity = Identity{Version: "0.0.0-test", Build: "build-test", ImageID: "img-test"}
	}
	// Subscribe to the software-fact topic BEFORE starting Run, so we
	// catch the initial publish without racing the goroutine's bus
	// subscriptions. The probe lives on its own connection so it
	// doesn't interfere with the caller's subscriptions.
	probeConn := b.NewConnection("updater-probe")
	probe := probeConn.Subscribe(TopicSoftwareFact)
	svc := New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	go svc.Run(ctx)
	select {
	case msg := <-probe.Channel():
		if msg == nil {
			t.Fatal("nil software fact at boot")
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("updater service did not publish initial software fact")
	}
	probeConn.Unsubscribe(probe)
	return svc, cancel
}

func TestPublishesInitialFactsOnRun(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	swSub := observer.Subscribe(TopicSoftwareFact)
	defer observer.Unsubscribe(swSub)
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	hSub := observer.Subscribe(TopicHealthFact)
	defer observer.Unsubscribe(hSub)

	_, cancel := runService(t, b, Options{
		Conn:     conn,
		Identity: Identity{Version: "1.2.3", Build: "abc", ImageID: "img-1"},
	})
	defer cancel()

	sw := waitForFact[SoftwareFact](t, swSub, nil)
	if sw.Version != "1.2.3" || sw.BuildID != "abc" || sw.ImageID != "img-1" {
		t.Fatalf("software identity wrong: %+v", sw)
	}
	if len(sw.BootID) != 16 {
		t.Fatalf("boot_id len = %d, want 16 chars: %q", len(sw.BootID), sw.BootID)
	}

	up := waitForFact[UpdaterFact](t, upSub, nil)
	if up.State != StateRunning {
		t.Fatalf("updater state = %q, want %q", up.State, StateRunning)
	}

	h := waitForFact[HealthFact](t, hSub, nil)
	if h.State != "ok" {
		t.Fatalf("health state = %q, want ok", h.State)
	}
}

func TestBootBuyFailurePublishesInitialUpdaterFailure(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)

	_, cancel := runService(t, b, Options{
		Conn:      conn,
		BootBuyRC: -42,
	})
	defer cancel()

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if strValue(up.LastError) != ErrABUpdateBuyFailed {
		t.Fatalf("last_error = %q, want %q", strValue(up.LastError), ErrABUpdateBuyFailed)
	}
	if int32Value(up.BootBuyRC) != -42 {
		t.Fatalf("boot_buy_rc = %d, want -42", int32Value(up.BootBuyRC))
	}
}

func TestBootBuyFailureRepublishPreservesFields(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)

	_, cancel := runService(t, b, Options{
		Conn:      conn,
		BootBuyRC: -7,
	})
	defer cancel()

	_ = waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })

	publisher := b.NewConnection("fabric-test")
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": true, "established": true, "peer_sid": "cm5-a", "local_sid": "mcu-a"},
		true,
	))

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool {
		return f.State == StateFailed && int32Value(f.BootBuyRC) == -7
	})
	if strValue(up.LastError) != ErrABUpdateBuyFailed {
		t.Fatalf("last_error = %q, want %q", strValue(up.LastError), ErrABUpdateBuyFailed)
	}
}

func TestBootBuyRCClearsOnExplicitUpdaterTransition(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)

	svc, cancel := runService(t, b, Options{
		Conn:      conn,
		BootBuyRC: -9,
	})
	defer cancel()

	_ = waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	svc.transitionTo(StatePreparing, "", "")

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StatePreparing })
	if up.BootBuyRC != nil {
		t.Fatalf("boot_buy_rc = %d, want nil after transition", *up.BootBuyRC)
	}
	if strValue(up.LastError) != "" {
		t.Fatalf("last_error = %q, want empty after transition", strValue(up.LastError))
	}
}

func TestPrepareTransitionsToReady(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()

	// drain initial running fact
	_ = waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateRunning })

	req := caller.NewMessage(TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU}, false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)

	select {
	case msg := <-replySub.Channel():
		reply, ok := msg.Payload.(PrepareReply)
		if !ok {
			t.Fatalf("reply payload type = %T", msg.Payload)
		}
		if !reply.Ready || reply.Target != TargetUpdaterMain || reply.MaxChunkSize != DefaultMaxChunkSize {
			t.Fatalf("prepare reply = %+v, want ready target max_chunk_size", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prepare reply")
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateReady })
	if up.LastError != nil {
		t.Fatalf("last_error not cleared on prepare: %q", strValue(up.LastError))
	}
}

func TestPrepareEmitsGenerationBreadcrumbs(t *testing.T) {
	diag := captureUpdaterDiag(t)
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()

	req := caller.NewMessage(TopicPrepareRPC, PrepareRequest{
		Target:          PrepareTargetMCU,
		JobID:           "job-diag",
		ExpectedImageID: "image-diag",
	}, false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)
	select {
	case msg := <-replySub.Channel():
		if _, ok := msg.Payload.(PrepareReply); !ok {
			t.Fatalf("prepare reply = %#v, want PrepareReply", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prepare reply")
	}

	lines := diag.snapshot()
	assertUpdaterDiagContains(t, lines, "[updater-stream]", "xfer_id -", "ev prepare_rx", "job_id job-diag", "expected_image_id image-diag")
	assertUpdaterDiagContains(t, lines, "[updater-stream]", "xfer_id -", "ev prepare_generation", "generation 1")
	assertUpdaterDiagContains(t, lines, "[updater-stream]", "xfer_id -", "ev prepare_done", "generation 1")
}

func prepareUpdaterForLease(t *testing.T, caller *bus.Connection) {
	t.Helper()
	req := caller.NewMessage(TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU}, false)
	sub := caller.Request(req)
	defer caller.Unsubscribe(sub)
	select {
	case msg := <-sub.Channel():
		if msg == nil {
			t.Fatal("nil prepare reply")
		}
		reply, ok := msg.Payload.(PrepareReply)
		if !ok || !reply.Ready {
			t.Fatalf("prepare reply = %#v, want ready", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prepare reply")
	}
}

func requestUpdaterReply(t *testing.T, caller *bus.Connection, topic bus.Topic, payload any) any {
	t.Helper()
	msg := caller.NewMessage(topic, payload, false)
	sub := caller.Request(msg)
	defer caller.Unsubscribe(sub)
	select {
	case rep := <-sub.Channel():
		if rep == nil {
			t.Fatal("nil reply")
		}
		return rep.Payload
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reply")
	}
	return nil
}

func TestBeginStreamedStageBeforePrepareReturnsStageNotPrepared(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()

	if gen, err := BeginStreamedStage("xfer-before-prepare", 4); err == nil || err.Error() != "stage_not_prepared" || gen != 0 {
		t.Fatalf("BeginStreamedStage before prepare = gen=%d err=%v, want stage_not_prepared", gen, err)
	}
}

func TestPrepareOpensSingleReceivingStreamLeaseAndClearsStaleDescriptor(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	memMD := NewMemoryMetadata()
	_ = memMD.WriteStagedDescriptor(StagedDescriptor{Version: "old", ImageID: "old-image", PayloadSHA256: "old"})
	_, cancel := runService(t, b, Options{Conn: conn, Metadata: memMD, MetadataWrite: memMD})
	defer cancel()

	prepareUpdaterForLease(t, caller)
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("prepare did not clear stale staged descriptor")
	}

	gen, err := BeginStreamedStage("xfer-lease", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if gen == 0 {
		t.Fatal("BeginStreamedStage returned generation 0")
	}
	defer CancelStreamedStage("xfer-lease", gen, "test_done")
	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateReceiving })
	if strValue(up.LastError) != "" {
		t.Fatalf("receiving last_error = %q, want empty", strValue(up.LastError))
	}

	if _, err := BeginStreamedStage("xfer-second", 4); err == nil || err.Error() != ErrBusy {
		t.Fatalf("second BeginStreamedStage err = %v, want busy", err)
	}
	if err := CommitBufferedStage("wrong-xfer", gen); err == nil || err.Error() != "stage_generation_mismatch" {
		t.Fatalf("wrong xfer CommitBufferedStage err = %v, want generation mismatch", err)
	}
	if err := CommitBufferedStage("xfer-lease", gen+1); err == nil || err.Error() != "stage_generation_mismatch" {
		t.Fatalf("wrong generation CommitBufferedStage err = %v, want generation mismatch", err)
	}
	if err := CommitBufferedStage("xfer-lease", gen); err != nil {
		t.Fatalf("matching CommitBufferedStage: %v", err)
	}
}

func TestPrepareAndCommitRejectWhileStreamLeaseActive(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := BeginStreamedStage("xfer-active", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	defer CancelStreamedStage("xfer-active", gen, "test_done")

	prepPayload := requestUpdaterReply(t, caller, TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU})
	prepReply, ok := prepPayload.(Reply)
	if !ok || prepReply.OK || prepReply.Error != ErrBusy {
		t.Fatalf("prepare while receiving = %#v, want busy", prepPayload)
	}

	commitPayload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	commitReply, ok := commitPayload.(Reply)
	if !ok || commitReply.OK || commitReply.Error != ErrBusy {
		t.Fatalf("commit while stream active = %#v, want busy", commitPayload)
	}
}

func TestStreamedStageDiagHookClearsOnBufferedCommit(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := BeginStreamedStage("xfer-hook-commit", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if !abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook inactive after BeginStreamedStage")
	}
	if err := CommitBufferedStage("xfer-hook-commit", gen); err != nil {
		t.Fatalf("CommitBufferedStage: %v", err)
	}
	if abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook still active after buffered commit")
	}
}

func TestStreamedStageDiagHookClearsOnAbort(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := BeginStreamedStage("xfer-hook-abort", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if !abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook inactive after BeginStreamedStage")
	}
	AbortStreamedStage("xfer-hook-abort", gen, "test_abort")
	if abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook still active after abort")
	}
}

func TestStreamedStageDiagHookClearsOnCommitError(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := BeginStreamedStage("xfer-hook-commit-error", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if !abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook inactive after BeginStreamedStage")
	}
	if _, err := CommitStreamedStage("xfer-hook-commit-error", gen); err == nil {
		t.Fatal("CommitStreamedStage returned nil error, want host streamed_stage_not_supported")
	}
	if abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook still active after commit error")
	}
}

func TestPrepareRejectsWhileCommittingOrRebooting(t *testing.T) {
	for _, state := range []State{StateCommitting, StateRebooting} {
		t.Run(string(state), func(t *testing.T) {
			b := newTestBus()
			conn := b.NewConnection("updater")
			caller := b.NewConnection("caller")
			svc, cancel := runService(t, b, Options{Conn: conn})
			defer cancel()

			svc.transitionTo(state, "", "9.9.9")
			payload := requestUpdaterReply(t, caller, TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU})
			reply, ok := payload.(Reply)
			if !ok || reply.OK || reply.Error != ErrBusy {
				t.Fatalf("prepare while %s = %#v, want busy", state, payload)
			}
		})
	}
}

func TestCancelStreamedStagePreventsLateStageSuccess(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	memMD := NewMemoryMetadata()
	verif := &fakeVerifierAccept{manifest: Manifest{
		Version:       "9.9.9",
		ImageID:       "ix",
		PayloadSHA256: strings.Repeat("a", 64),
		PayloadLength: 4,
	}}

	_, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := BeginStreamedStage("xfer-cancel", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if err := CommitBufferedStage("xfer-cancel", gen); err != nil {
		t.Fatalf("CommitBufferedStage: %v", err)
	}
	CancelStreamedStage("xfer-cancel", gen, "test_cancel")

	stage := testStagePayload("xfer-cancel", []byte("blob"))
	stage.Generation = gen
	payload := requestUpdaterReply(t, caller, TopicStageRPC, stage)
	reply, ok := payload.(StageReply)
	if !ok || reply.OK {
		t.Fatalf("late stage after cancel = %#v, want rejection", payload)
	}
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("late stage after cancel persisted staged descriptor")
	}
}

func TestReleasedStagedLeaseIgnoresLateCancel(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)
	memMD := NewMemoryMetadata()
	app := &fakeApplier{rebootCh: make(chan StagedDescriptor, 1)}
	verif := &fakeVerifierAccept{manifest: Manifest{
		Version:       "9.9.9",
		BuildID:       "build-9.9.9",
		ImageID:       "mcu-dev-9.9.9",
		PayloadSHA256: strings.Repeat("c", 64),
		PayloadLength: 4,
	}}

	_, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Applier:       app,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := BeginStreamedStage("xfer-released", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if err := CommitBufferedStage("xfer-released", gen); err != nil {
		t.Fatalf("CommitBufferedStage: %v", err)
	}

	stage := testStagePayload("xfer-released", []byte("blob"))
	stage.Generation = gen
	payload := requestUpdaterReply(t, caller, TopicStageRPC, stage)
	reply, ok := payload.(StageReply)
	if !ok || !reply.OK {
		t.Fatalf("stage reply = %#v, want ok", payload)
	}
	waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateStaged })
	if _, ok := memMD.StagedDescriptor(); !ok {
		t.Fatal("stage did not persist descriptor")
	}

	CancelStreamedStage("xfer-released", gen, "late_cancel")
	if _, ok := memMD.StagedDescriptor(); !ok {
		t.Fatal("late cancel cleared released staged descriptor")
	}

	commitPayload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	commit, ok := commitPayload.(CommitReply)
	if !ok || !commit.Accepted || !commit.RebootRequired {
		t.Fatalf("commit after late cancel = %#v, want accepted", commitPayload)
	}
}

func TestStaleGenerationAndWrongXferCannotMutateStreamedStage(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	memMD := NewMemoryMetadata()
	verif := &fakeVerifierAccept{manifest: Manifest{
		Version:       "9.9.9",
		ImageID:       "ix",
		PayloadSHA256: strings.Repeat("b", 64),
		PayloadLength: 4,
	}}

	_, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := BeginStreamedStage("xfer-current", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	defer CancelStreamedStage("xfer-current", gen, "test_done")

	if err := WriteStreamedStage("wrong-xfer", gen, []byte("data")); err == nil || err.Error() != "stage_generation_mismatch" {
		t.Fatalf("wrong xfer WriteStreamedStage err = %v, want generation mismatch", err)
	}
	if _, err := CommitStreamedStage("xfer-current", gen+1); err == nil || err.Error() != "stage_generation_mismatch" {
		t.Fatalf("stale generation CommitStreamedStage err = %v, want generation mismatch", err)
	}
	if err := CommitBufferedStage("xfer-current", gen); err != nil {
		t.Fatalf("CommitBufferedStage: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   string
		gen  uint64
	}{
		{name: "wrong_xfer", id: "wrong-xfer", gen: gen},
		{name: "stale_generation", id: "xfer-current", gen: gen + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := testStagePayload(tc.id, []byte("blob"))
			stage.Generation = tc.gen
			payload := requestUpdaterReply(t, caller, TopicStageRPC, stage)
			reply, ok := payload.(StageReply)
			if !ok || reply.OK {
				t.Fatalf("stage with stale identity = %#v, want rejection", payload)
			}
			if _, ok := memMD.StagedDescriptor(); ok {
				t.Fatal("stage with stale identity persisted descriptor")
			}
		})
	}
}

func TestCommitWithoutStagedReturnsNothingStaged(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()

	req := caller.NewMessage(TopicCommitRPC, CommitRequest{}, false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)

	select {
	case msg := <-replySub.Channel():
		reply, ok := msg.Payload.(Reply)
		if !ok {
			t.Fatalf("reply payload type = %T", msg.Payload)
		}
		if reply.OK {
			t.Fatalf("commit unexpectedly OK without staged image: %+v", reply)
		}
		if reply.Error != ErrNothingStaged {
			t.Fatalf("commit error = %q, want %q", reply.Error, ErrNothingStaged)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for commit reply")
	}
}

func TestCommitWithoutStagedStateRefusesEvenWithDescriptor(t *testing.T) {
	// Both halves of the staged condition are required: a descriptor
	// in metadata AND state == staged. A descriptor without the
	// matching state means the receiver didn't actually finish, so
	// commit must refuse rather than push into committing/rebooting.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	md := &fakeMetadata{
		has:    true,
		staged: StagedDescriptor{Version: "9.9.9", BuildID: "bx", ImageID: "ix", Length: 4096, Slot: 1, PayloadSHA256: strings.Repeat("a", 64)},
	}
	_, cancel := runService(t, b, Options{Conn: conn, Metadata: md, Applier: &fakeApplier{}})
	defer cancel()

	req := caller.NewMessage(TopicCommitRPC, CommitRequest{}, false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)
	select {
	case msg := <-replySub.Channel():
		reply, _ := msg.Payload.(Reply)
		if reply.OK || reply.Error != ErrNothingStaged {
			t.Fatalf("commit reply = %+v, want refusal=nothing_staged", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for commit reply")
	}
}

func TestCommitWithoutApplierReturnsApplyUnavailable(t *testing.T) {
	// Spec safety: the commit RPC must not claim success when the MCU
	// has no apply hook wired (the production default RefusingApplier
	// returns ErrApplyUnavailable). State stays at staged; the
	// receiver-staged descriptor remains valid for a subsequent
	// commit once a real Applier is wired.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	verif := &fakeVerifierAccept{manifest: Manifest{Version: "9.9.9", PayloadSHA256: strings.Repeat("a", 64), PayloadLength: 4}}
	memMD := NewMemoryMetadata()
	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Metadata:      memMD,
		MetadataWrite: memMD,
		// No Applier supplied — defaults to RefusingApplier.
	})
	defer cancel()

	// Drive updater/main staging to staged state.
	rreq := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "xfer-x", []byte("blob")), false)
	rsub := caller.Request(rreq)
	defer caller.Unsubscribe(rsub)
	<-rsub.Channel()

	creq := caller.NewMessage(TopicCommitRPC, CommitRequest{}, false)
	csub := caller.Request(creq)
	defer caller.Unsubscribe(csub)
	select {
	case msg := <-csub.Channel():
		reply, _ := msg.Payload.(Reply)
		if reply.OK || reply.Error != ErrApplyUnavailable {
			t.Fatalf("commit reply = %+v, want refusal=apply_unavailable", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for commit reply")
	}

	// State must NOT have transitioned to committing/rebooting — that would lie.
	settle := time.After(150 * time.Millisecond)
	for {
		select {
		case msg := <-upSub.Channel():
			fact, _ := msg.Payload.(UpdaterFact)
			if fact.State == StateCommitting || fact.State == StateRebooting {
				t.Fatalf("state transitioned to %s despite refusing applier", fact.State)
			}
		case <-settle:
			return
		}
	}
}

func TestCommitWithFakeApplierTransitionsToRebooting(t *testing.T) {
	// With a real Applier supplied, the staged descriptor in metadata and state
	// drives commit through committing to rebooting.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	verif := &fakeVerifierAccept{manifest: Manifest{Version: "9.9.9", PayloadSHA256: strings.Repeat("a", 64), PayloadLength: 4}}
	memMD := NewMemoryMetadata()
	app := &fakeApplier{rebootCh: make(chan StagedDescriptor, 1)}
	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Applier:       app,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()

	// Stage via updater/main.
	rreq := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "x", []byte("blob")), false)
	<-caller.Request(rreq).Channel()

	// Commit.
	creq := caller.NewMessage(TopicCommitRPC, CommitRequest{}, false)
	csub := caller.Request(creq)
	defer caller.Unsubscribe(csub)
	select {
	case msg := <-csub.Channel():
		reply, _ := msg.Payload.(CommitReply)
		if !reply.Accepted || !reply.RebootRequired {
			t.Fatalf("commit reply = %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}

	select {
	case <-app.rebootCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ArmReboot")
	}

	canCalls, rebootCalls := app.callCounts()
	if canCalls != 1 || rebootCalls != 1 {
		t.Fatalf("Applier hooks fired wrong: can=%d reboot=%d, want 1+1",
			canCalls, rebootCalls)
	}
	if got := app.rebootCall(0).Version; got != "9.9.9" {
		t.Fatalf("ArmReboot got descriptor.Version = %q, want 9.9.9", got)
	}
	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateRebooting })
	if strValue(up.PendingVersion) != "9.9.9" {
		t.Fatalf("pending_version = %q", strValue(up.PendingVersion))
	}
}

func TestCommitApplyRebootErrorPublishesFailedAfterAcceptedReply(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	verif := &fakeVerifierAccept{manifest: Manifest{
		Version:       "9.9.9",
		BuildID:       "build-9.9.9",
		ImageID:       "mcu-dev-9.9.9",
		PayloadSHA256: strings.Repeat("a", 64),
		PayloadLength: 4,
	}}
	memMD := NewMemoryMetadata()
	app := &fakeApplier{rebootErr: errors.New("apply_reboot_failed:reboot_into_slot:-1")}
	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Applier:       app,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()

	rreq := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "x", []byte("blob")), false)
	<-caller.Request(rreq).Channel()

	creq := caller.NewMessage(TopicCommitRPC, CommitRequest{}, false)
	csub := caller.Request(creq)
	defer caller.Unsubscribe(csub)
	select {
	case msg := <-csub.Channel():
		reply, _ := msg.Payload.(CommitReply)
		if !reply.Accepted || !reply.RebootRequired {
			t.Fatalf("commit reply = %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for commit reply")
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if got := strValue(up.LastError); got != "apply_reboot_failed:reboot_into_slot:-1" {
		t.Fatalf("last_error = %q", got)
	}
}

func TestApplyRebootErrorIgnoredWhenContextNoLongerMatches(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	svc, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()

	desc := StagedDescriptor{Version: "9.9.9", ImageID: "mcu-dev-9.9.9"}
	svc.setStagedImage(desc.ImageID, desc.Version)
	svc.transitionTo(StateRebooting, "", desc.Version)
	svc.transitionTo(StateReady, "", "")
	svc.applyResults <- applyRebootResult{
		desc: desc,
		err:  errors.New("apply_reboot_failed:reboot_into_slot:-1"),
	}

	settle := time.After(150 * time.Millisecond)
	for {
		select {
		case msg := <-upSub.Channel():
			fact, _ := msg.Payload.(UpdaterFact)
			if fact.State == StateFailed {
				t.Fatalf("stale apply result unexpectedly failed updater: %+v", fact)
			}
		case <-settle:
			return
		}
	}
}

// ---- updater/main staging path with fakes ----------------------------

func TestStageStubVerifierPublishesFailed(t *testing.T) {
	// Production stub: any artefact is rejected. State must transition
	// to failed with last_error matching the sentinel.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	svc, cancel := runService(t, b, Options{Conn: conn, Verifier: StubVerifier()})
	defer cancel()

	req := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "xfer-1", []byte("blob")), false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)

	select {
	case msg := <-replySub.Channel():
		reply, ok := msg.Payload.(StageReply)
		if !ok || reply.OK {
			t.Fatalf("stage unexpectedly OK with stub: %+v", reply)
		}
		if !strings.Contains(reply.Err, "verifier_stub") {
			t.Fatalf("stage err = %q, want stub sentinel", reply.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stage reply")
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if !strings.Contains(strValue(up.LastError), "verifier_stub") {
		t.Fatalf("last_error = %q, want stub sentinel", strValue(up.LastError))
	}
}

func TestStageSignedImageVerifierWritesManifestDescriptor(t *testing.T) {
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	target := imagev1.Target{
		ProductFamily:   "bigbox",
		HardwareProfile: "bb-v1-cm5-2",
		MCUBoardFamily:  "rp2354a",
	}
	artefact, _, err := imagev1.Pack([]byte("signed payload"), imagev1.PackOptions{
		Target:  target,
		Version: "13.0",
		BuildID: "build-13.0",
		ImageID: "mcu-dev-13.0",
		KeyID:   "test-key",
	}, priv)
	if err != nil {
		t.Fatal(err)
	}

	oldProduct := SignedImageProductFamily
	oldProfile := SignedImageHardwareProfile
	oldBoard := SignedImageMCUBoardFamily
	oldKeyID := SignedImageTrustedKeyID
	oldKey := SignedImageTrustedPublicKey
	defer func() {
		SignedImageProductFamily = oldProduct
		SignedImageHardwareProfile = oldProfile
		SignedImageMCUBoardFamily = oldBoard
		SignedImageTrustedKeyID = oldKeyID
		SignedImageTrustedPublicKey = oldKey
	}()
	SignedImageProductFamily = target.ProductFamily
	SignedImageHardwareProfile = target.HardwareProfile
	SignedImageMCUBoardFamily = target.MCUBoardFamily
	SignedImageTrustedKeyID = "test-key"
	SignedImageTrustedPublicKey = hex.EncodeToString(pub)

	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	memMD := NewMemoryMetadata()
	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      SignedImageVerifier(),
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()

	req := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "signed-xfer", artefact), false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)
	select {
	case msg := <-replySub.Channel():
		reply, _ := msg.Payload.(StageReply)
		if !reply.OK {
			t.Fatalf("stage reply not ok: %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stage reply")
	}

	desc, ok := memMD.StagedDescriptor()
	if !ok {
		t.Fatal("staged descriptor not persisted")
	}
	if desc.Version != "13.0" || desc.BuildID != "build-13.0" || desc.ImageID != "mcu-dev-13.0" {
		t.Fatalf("descriptor wrong: %+v", desc)
	}
	if desc.Length != uint32(len("signed payload")) || len(desc.PayloadSHA256) != 64 {
		t.Fatalf("descriptor payload metadata wrong: %+v", desc)
	}
}

func TestStageFakeAcceptWritesStagedDescriptor(t *testing.T) {
	// On verifier success, staging writes the manifest fields to the
	// metadata writer. A subsequent commit RPC reads the descriptor back
	// via the matching reader and transitions to rebooting with the same
	// pending_version.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	verif := &fakeVerifierAccept{manifest: Manifest{
		Version:       "9.9.9",
		BuildID:       "bx",
		ImageID:       "ix",
		PayloadSHA256: "deadbeef",
		PayloadLength: 4,
	}}
	memMD := NewMemoryMetadata()
	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Applier:       &fakeApplier{}, // success path; production default refuses
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()

	// Drive updater/main staging to verifier success.
	req := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "xfer-w11", []byte("blob")), false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)
	select {
	case msg := <-replySub.Channel():
		reply, _ := msg.Payload.(StageReply)
		if !reply.OK {
			t.Fatalf("stage reply not ok: %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stage reply")
	}

	// Reader sees the staged descriptor + its embedded payload hash.
	desc, ok := memMD.StagedDescriptor()
	if !ok {
		t.Fatal("staged descriptor not persisted")
	}
	if desc.Version != "9.9.9" || desc.PayloadSHA256 != "deadbeef" || desc.Length != 4 {
		t.Fatalf("descriptor wrong: %+v", desc)
	}
	// WriteStagedDescriptor must not promote the staged hash into the
	// running-image hash. Running hash stays "" until SetRunningPayloadSHA is
	// called at the next boot.
	if got := memMD.PayloadSHA256(); got != "" {
		t.Fatalf("running payload_sha256 leaked from staged descriptor: %q", got)
	}

	// Commit RPC now succeeds because the reader sees the descriptor
	// AND state is staged AND a real Applier is wired.
	creq := caller.NewMessage(TopicCommitRPC, CommitRequest{}, false)
	csub := caller.Request(creq)
	defer caller.Unsubscribe(csub)
	select {
	case msg := <-csub.Channel():
		reply, _ := msg.Payload.(CommitReply)
		if !reply.Accepted {
			t.Fatalf("commit reply not ok: %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for commit reply")
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateRebooting })
	if strValue(up.PendingVersion) != "9.9.9" {
		t.Fatalf("pending_version = %q, want 9.9.9", strValue(up.PendingVersion))
	}
}

func TestStageFailureClearsStaleStagedDescriptor(t *testing.T) {
	// A (stage A) -> (prepare for B) -> (stage B fails) flow must not leave
	// descriptor A persisted. The next commit should return nothing_staged
	// rather than committing stale firmware.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	// Pre-stage: a real descriptor sitting in metadata from an earlier
	// successful flow.
	memMD := NewMemoryMetadata()
	_ = memMD.WriteStagedDescriptor(StagedDescriptor{Version: "1.0.0", PayloadSHA256: "old"})

	// Service uses a verifier that always rejects.
	verif := &fakeVerifierReject{err: errString("bad_signature")}
	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Applier:       &fakeApplier{},
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()

	// Drive updater/main staging to failure.
	rreq := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "x", []byte("blob")), false)
	rsub := caller.Request(rreq)
	defer caller.Unsubscribe(rsub)
	select {
	case <-rsub.Channel():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}

	// The stale descriptor must have been cleared.
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatalf("stale staged descriptor survived receiver failure")
	}

	// Commit must refuse with nothing_staged rather than commit the
	// stale image.
	creq := caller.NewMessage(TopicCommitRPC, CommitRequest{}, false)
	csub := caller.Request(creq)
	defer caller.Unsubscribe(csub)
	select {
	case msg := <-csub.Channel():
		reply, _ := msg.Payload.(Reply)
		if reply.OK || reply.Error != ErrNothingStaged {
			t.Fatalf("commit reply = %+v, want refusal=nothing_staged", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestPrepareClearsStaleStagedDescriptor(t *testing.T) {
	// A new prepare invalidates any prior persisted stage so a partial-
	// failure subsequent transfer can't accidentally commit the
	// previously-staged image.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	memMD := NewMemoryMetadata()
	_ = memMD.WriteStagedDescriptor(StagedDescriptor{Version: "1.0.0", PayloadSHA256: "old"})

	_, cancel := runService(t, b, Options{
		Conn:          conn,
		Metadata:      memMD,
		MetadataWrite: memMD,
		Applier:       &fakeApplier{},
	})
	defer cancel()

	preq := caller.NewMessage(TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU}, false)
	psub := caller.Request(preq)
	defer caller.Unsubscribe(psub)
	select {
	case msg := <-psub.Channel():
		reply, _ := msg.Payload.(PrepareReply)
		if !reply.Ready {
			t.Fatalf("prepare reply = %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}

	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatalf("stale staged descriptor survived prepare")
	}
}

func TestPrepareClearFailureTransitionsToFailedAndAllowsRetry(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	memMD := NewMemoryMetadata()
	_ = memMD.WriteStagedDescriptor(StagedDescriptor{Version: "1.0.0", PayloadSHA256: "old"})
	failMD := &failingClearMetadata{MemoryMetadata: memMD, err: errors.New("flash_busy")}

	_, cancel := runService(t, b, Options{
		Conn:          conn,
		Metadata:      memMD,
		MetadataWrite: failMD,
		Applier:       &fakeApplier{},
	})
	defer cancel()

	_ = waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateRunning })

	preq := caller.NewMessage(TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU}, false)
	psub := caller.Request(preq)
	defer caller.Unsubscribe(psub)
	select {
	case msg := <-psub.Channel():
		reply, ok := msg.Payload.(Reply)
		if !ok {
			t.Fatalf("reply payload type = %T", msg.Payload)
		}
		if reply.OK || reply.Error != "metadata_clear_failed:flash_busy" {
			t.Fatalf("prepare reply = %+v, want metadata clear failure", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for failed prepare reply")
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if strValue(up.LastError) != "metadata_clear_failed:flash_busy" {
		t.Fatalf("last_error = %q, want metadata clear failure", strValue(up.LastError))
	}

	failMD.err = nil
	retry := caller.NewMessage(TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU}, false)
	retrySub := caller.Request(retry)
	defer caller.Unsubscribe(retrySub)
	select {
	case msg := <-retrySub.Channel():
		reply, ok := msg.Payload.(PrepareReply)
		if !ok {
			t.Fatalf("retry reply payload type = %T", msg.Payload)
		}
		if !reply.Ready {
			t.Fatalf("retry prepare reply = %+v, want ready", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for retry prepare reply")
	}
}

func TestStageFakeAcceptPublishesStaged(t *testing.T) {
	// Test fake exercises the success path. State -> staged,
	// pending_version mirrors the manifest's build version, reply.OK = true.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	verif := &fakeVerifierAccept{manifest: Manifest{Version: "9.9.9", BuildID: "bx", ImageID: "ix", PayloadSHA256: strings.Repeat("a", 64), PayloadLength: 4}}
	svc, cancel := runService(t, b, Options{Conn: conn, Verifier: verif})
	defer cancel()

	req := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "xfer-2", []byte("blob")), false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)

	select {
	case msg := <-replySub.Channel():
		reply, ok := msg.Payload.(StageReply)
		if !ok || !reply.OK || reply.Stage != "staged" {
			t.Fatalf("stage reply = %+v ok-type=%v", reply, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stage reply")
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateStaged })
	if strValue(up.PendingVersion) != "9.9.9" {
		t.Fatalf("pending_version = %q, want 9.9.9", strValue(up.PendingVersion))
	}
}

func TestStageFakeRejectPublishesFailed(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

	verif := &fakeVerifierReject{err: errString("manifest_check_failed")}
	svc, cancel := runService(t, b, Options{Conn: conn, Verifier: verif})
	defer cancel()

	req := caller.NewMessage(TopicStageRPC, preparedStagePayload(t, caller, svc, "xfer-3", []byte("blob")), false)
	replySub := caller.Request(req)
	defer caller.Unsubscribe(replySub)

	select {
	case msg := <-replySub.Channel():
		reply, ok := msg.Payload.(StageReply)
		if !ok || reply.OK {
			t.Fatalf("stage unexpectedly OK: %+v", reply)
		}
		if reply.Err != "manifest_check_failed" {
			t.Fatalf("stage err = %q, want manifest_check_failed", reply.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stage reply")
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if strValue(up.LastError) != "manifest_check_failed" {
		t.Fatalf("last_error = %q, want manifest_check_failed", strValue(up.LastError))
	}
}

func TestRepublishReasonTracksReadyAndSessionEdges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hadPrev bool
		prev    linkObservation
		cur     linkObservation
		want    string
	}{
		{
			name: "first ready",
			cur:  linkObservation{Ready: true, PeerSID: "peer-1", LocalSID: "local-1"},
			want: "ready_edge",
		},
		{
			name:    "false to true",
			hadPrev: true,
			prev:    linkObservation{Ready: false, PeerSID: "peer-1", LocalSID: "local-1"},
			cur:     linkObservation{Ready: true, PeerSID: "peer-1", LocalSID: "local-1"},
			want:    "ready_edge",
		},
		{
			name:    "same ready identity",
			hadPrev: true,
			prev:    linkObservation{Ready: true, PeerSID: "peer-1", LocalSID: "local-1"},
			cur:     linkObservation{Ready: true, PeerSID: "peer-1", LocalSID: "local-1"},
		},
		{
			name:    "peer sid changed",
			hadPrev: true,
			prev:    linkObservation{Ready: true, PeerSID: "peer-1", LocalSID: "local-1"},
			cur:     linkObservation{Ready: true, PeerSID: "peer-2", LocalSID: "local-1"},
			want:    "peer_sid_changed",
		},
		{
			name:    "local sid changed",
			hadPrev: true,
			prev:    linkObservation{Ready: true, PeerSID: "peer-1", LocalSID: "local-1"},
			cur:     linkObservation{Ready: true, PeerSID: "peer-1", LocalSID: "local-2"},
			want:    "local_sid_changed",
		},
		{
			name:    "not ready",
			hadPrev: true,
			prev:    linkObservation{Ready: true, PeerSID: "peer-1", LocalSID: "local-1"},
			cur:     linkObservation{Ready: false, PeerSID: "peer-2", LocalSID: "local-2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := republishReason(tc.prev, tc.cur, tc.hadPrev); got != tc.want {
				t.Fatalf("republishReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRepublishOnLinkReadyAndSessionEdges(t *testing.T) {
	// The updater republishes its retained state/self/* surface on every
	// !Ready -> Ready transition, and on SID changes while Ready remains true.
	b := newTestBus()
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	swSub := observer.Subscribe(TopicSoftwareFact)
	defer observer.Unsubscribe(swSub)

	_, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()

	// Drain the initial software fact emitted on Run start.
	_ = waitForFact[SoftwareFact](t, swSub, nil)

	// Publish a link-state retain with Ready=false first; should not
	// trigger a republish.
	publisher := b.NewConnection("test-fabric")
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": false, "established": false, "peer_sid": "cm5-x", "local_sid": "mcu-a"},
		true,
	))
	// Brief wait then drop everything that's already in the channel.
	time.Sleep(50 * time.Millisecond)
	for len(swSub.Channel()) > 0 {
		<-swSub.Channel()
	}

	// Now flip Ready to true: the !Ready -> Ready edge MUST trigger a
	// software-fact republish.
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": true, "established": true, "peer_sid": "cm5-x", "local_sid": "mcu-a"},
		true,
	))
	_ = waitForFact[SoftwareFact](t, swSub, nil)

	// Subsequent Ready=true retain (no edge) should NOT trigger another
	// immediate edge republish. The default level-triggered cadence does
	// not fire inside this short settle window.
	publisher.Publish(publisher.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": true, "established": true, "peer_sid": "cm5-x", "local_sid": "mcu-a", "last_rx_ms": int64(123)},
		true,
	))
	assertNoSoftwareRepublish(t, swSub, 150*time.Millisecond)

	publisher.Publish(publisher.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": true, "established": true, "peer_sid": "cm5-y", "local_sid": "mcu-a"},
		true,
	))
	_ = waitForFact[SoftwareFact](t, swSub, nil)

	publisher.Publish(publisher.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": true, "established": true, "peer_sid": "cm5-y", "local_sid": "mcu-b"},
		true,
	))
	_ = waitForFact[SoftwareFact](t, swSub, nil)
}

func TestCriticalFactsRepublishOnUpdaterStateChange(t *testing.T) {
	b := bus.NewBus(32, "+", "#")
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	swSub := observer.Subscribe(TopicSoftwareFact)
	defer observer.Unsubscribe(swSub)
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	hSub := observer.Subscribe(TopicHealthFact)
	defer observer.Unsubscribe(hSub)

	svc, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)

	svc.transitionTo(StateReady, "", "")
	_, up, h := waitForCriticalFactSet(t, swSub, upSub, hSub)
	if up.State != StateReady {
		t.Fatalf("updater state = %q, want %q", up.State, StateReady)
	}
	if h.State != "ok" {
		t.Fatalf("health state = %q, want ok", h.State)
	}
}

func TestCriticalRepublishCadenceWhileReady(t *testing.T) {
	b := bus.NewBus(64, "+", "#")
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	swSub := observer.Subscribe(TopicSoftwareFact)
	defer observer.Unsubscribe(swSub)
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	hSub := observer.Subscribe(TopicHealthFact)
	defer observer.Unsubscribe(hSub)

	_, cancel := runService(t, b, Options{
		Conn:              conn,
		CriticalRepublish: testCriticalRepublishConfig(),
	})
	defer cancel()
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)

	publisher := b.NewConnection("fabric-test")
	publishTestLinkState(publisher, true, "cm5-a", "mcu-a")

	// Ready edge publishes immediately, then the level-triggered cadence
	// keeps publishing the three critical facts while the link remains ready.
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)
}

func TestCriticalRepublishCadenceStopsWhenLinkNotReady(t *testing.T) {
	b := bus.NewBus(32, "+", "#")
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	swSub := observer.Subscribe(TopicSoftwareFact)
	defer observer.Unsubscribe(swSub)

	_, cancel := runService(t, b, Options{
		Conn: conn,
		CriticalRepublish: CriticalRepublishConfig{
			BurstInterval:  100 * time.Millisecond,
			BurstDuration:  200 * time.Millisecond,
			SteadyInterval: 100 * time.Millisecond,
		},
	})
	defer cancel()
	_ = waitForFact[SoftwareFact](t, swSub, nil)

	publisher := b.NewConnection("fabric-test")
	publishTestLinkState(publisher, true, "cm5-a", "mcu-a")
	_ = waitForFact[SoftwareFact](t, swSub, nil)

	publishTestLinkState(publisher, false, "cm5-a", "mcu-a")
	time.Sleep(20 * time.Millisecond)
	drainSubscription(swSub)
	assertNoSoftwareRepublish(t, swSub, 150*time.Millisecond)
}

func assertNoSoftwareRepublish(t *testing.T, sub *bus.Subscription, d time.Duration) {
	t.Helper()
	settled := time.After(d)
	for {
		select {
		case <-sub.Channel():
			t.Fatal("unexpected software fact republish")
		case <-settled:
			return
		}
	}
}

// ---- jsonDecode robustness ------------------------------------------

func TestJSONDecodeAcceptsTypedAndRaw(t *testing.T) {
	t1, ok := jsonDecode[PrepareRequest](PrepareRequest{Target: "x"})
	if !ok || t1.Target != "x" {
		t.Fatalf("typed: %v %v", ok, t1)
	}
	raw := json.RawMessage(`{"target":"y"}`)
	t2, ok := jsonDecode[PrepareRequest](raw)
	if !ok || t2.Target != "y" {
		t.Fatalf("raw: %v %v", ok, t2)
	}
	t3, ok := jsonDecode[PrepareRequest](nil)
	if !ok || t3.Target != "" {
		t.Fatalf("nil: %v %v", ok, t3)
	}
	t4, ok := jsonDecode[PrepareRequest]([]byte(`{"target":"z"}`))
	if !ok || t4.Target != "z" {
		t.Fatalf("bytes: %v %v", ok, t4)
	}
}

// ---- memorySink behaviour -------------------------------------------

func TestMemorySinkAbortClearsBuffer(t *testing.T) {
	s := &memorySink{}
	_, _ = s.Write([]byte("hello"))
	_ = s.Abort()
	if got := s.buf.Len(); got != 0 {
		t.Fatalf("after abort buf len = %d, want 0", got)
	}
}

func TestMemorySinkCommitClosesWrites(t *testing.T) {
	s := &memorySink{}
	_, _ = s.Write([]byte("hello"))
	if err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	_, err := s.Write([]byte("more"))
	if err != io.ErrClosedPipe {
		t.Fatalf("write after commit err = %v, want io.ErrClosedPipe", err)
	}
}

// errString is a tiny error type for tests that don't want to import
// the standard errors package twice.
type errString string

func (e errString) Error() string { return string(e) }

// Compile-time assert that bytes.NewReader satisfies the verifier API.
var _ io.Reader = bytes.NewReader(nil)

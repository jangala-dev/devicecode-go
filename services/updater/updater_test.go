package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"devicecode-go/bus"
	"github.com/jangala-dev/pico2-a-b/signedimage"
)

// ---- helpers --------------------------------------------------------

func newTestBus() *bus.Bus { return bus.NewBus(8, "+", "#") }

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
// commit RPC returns commit_failed until the real abupdate-backed
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
		case msg, ok := <-sub.Channel():
			if !ok {
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

func testCriticalFactConfig() CriticalFactConfig {
	return CriticalFactConfig{LivenessInterval: 30 * time.Millisecond}
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
	}
}

func preparedStreamedStageLease(t *testing.T, caller *bus.Connection, svc *Service, id string, artefact []byte) (StagePayload, uint64) {
	t.Helper()
	preparePayload := requestUpdaterReply(t, caller, TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU})
	switch reply := preparePayload.(type) {
	case PrepareReply:
		if !reply.Ready {
			t.Fatalf("prepare reply not ready: %+v", reply)
		}
	case Reply:
		t.Fatalf("prepare failed: %+v", reply)
	default:
		t.Fatalf("prepare payload type = %T", preparePayload)
	}
	generation, err := svc.BeginStreamedStage(id, uint32(len(artefact)))
	if err != nil {
		t.Fatalf("begin streamed stage: %v", err)
	}
	if len(artefact) > 0 {
		if err := svc.WriteStreamedStage(id, generation, artefact); err != nil {
			t.Fatalf("write streamed stage: %v", err)
		}
	}
	payload := testStagePayload(id, artefact)
	payload.Generation = generation
	return payload, generation
}

func preparedStagePayload(t *testing.T, caller *bus.Connection, svc *Service, id string, artefact []byte) StagePayload {
	t.Helper()
	payload, generation := preparedStreamedStageLease(t, caller, svc, id, artefact)
	if _, err := svc.CommitStreamedStage(id, generation); err != nil {
		t.Fatalf("commit streamed stage: %v", err)
	}
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
	case <-probe.Channel():
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

func TestBootBuyFailureLivenessPreservesFields(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)

	_, cancel := runService(t, b, Options{
		Conn:      conn,
		BootBuyRC: -7,
		CriticalFacts: CriticalFactConfig{
			LivenessInterval: 25 * time.Millisecond,
		},
	})
	defer cancel()

	initial := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if strValue(initial.LastError) != ErrABUpdateBuyFailed {
		t.Fatalf("initial last_error = %q, want %q", strValue(initial.LastError), ErrABUpdateBuyFailed)
	}
	if int32Value(initial.BootBuyRC) != -7 {
		t.Fatalf("initial boot_buy_rc = %d, want -7", int32Value(initial.BootBuyRC))
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool {
		return f.State == StateFailed && int32Value(f.BootBuyRC) == -7
	})
	if strValue(up.LastError) != ErrABUpdateBuyFailed {
		t.Fatalf("liveness last_error = %q, want %q", strValue(up.LastError), ErrABUpdateBuyFailed)
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

	payload := requestUpdaterReply(t, caller, TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU})
	reply, ok := payload.(PrepareReply)
	if !ok {
		t.Fatalf("reply payload type = %T", payload)
	}
	if !reply.Ready || reply.Target != TargetUpdaterMain || reply.MaxChunkSize != DefaultMaxChunkSize {
		t.Fatalf("prepare reply = %+v, want ready target max_chunk_size", reply)
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateReady })
	if up.LastError != nil {
		t.Fatalf("last_error not cleared on prepare: %q", strValue(up.LastError))
	}
}

func prepareUpdaterForLease(t *testing.T, caller *bus.Connection) {
	t.Helper()
	payload := requestUpdaterReply(t, caller, TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU})
	reply, ok := payload.(PrepareReply)
	if !ok || !reply.Ready {
		t.Fatalf("prepare reply = %#v, want ready", payload)
	}
}

func requestUpdaterReply(t *testing.T, caller *bus.Connection, topic bus.Topic, payload any) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := caller.Call(ctx, topic, payload)
	if err != nil {
		t.Fatalf("endpoint call %s failed: %v", topicString(topic), err)
	}
	return reply
}

func topicString(tp bus.Topic) string { return fmt.Sprint(tp) }

func TestStreamedStageControllerRequiresUpdaterRun(t *testing.T) {
	b := newTestBus()
	svc := New(Options{Conn: b.NewConnection("updater")})

	if gen, err := svc.BeginStreamedStage("xfer-not-running", 4); err == nil || err.Error() != "updater_not_running" || gen != 0 {
		t.Fatalf("BeginStreamedStage before Run = gen=%d err=%v, want updater_not_running", gen, err)
	}
}

func TestBeginStreamedStageBeforePrepareReturnsStageNotPrepared(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")

	svc, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()

	if gen, err := svc.BeginStreamedStage("xfer-before-prepare", 4); err == nil || err.Error() != "stage_not_prepared" || gen != 0 {
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
	svc, cancel := runService(t, b, Options{Conn: conn, Metadata: memMD, MetadataWrite: memMD})
	defer cancel()

	prepareUpdaterForLease(t, caller)
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("prepare did not clear stale staged descriptor")
	}

	gen, err := svc.BeginStreamedStage("xfer-lease", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if gen == 0 {
		t.Fatal("BeginStreamedStage returned generation 0")
	}
	defer svc.CancelStreamedStage("xfer-lease", gen, "test_done")
	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateReceiving })
	if strValue(up.LastError) != "" {
		t.Fatalf("receiving last_error = %q, want empty", strValue(up.LastError))
	}

	if _, err := svc.BeginStreamedStage("xfer-second", 4); err == nil || err.Error() != ErrBusy {
		t.Fatalf("second BeginStreamedStage err = %v, want busy", err)
	}
	if err := svc.markStreamedStageCommitted("wrong-xfer", gen); err == nil || err.Error() != "stage_generation_mismatch" {
		t.Fatalf("wrong xfer markStreamedStageCommitted err = %v, want generation mismatch", err)
	}
	if err := svc.markStreamedStageCommitted("xfer-lease", gen+1); err == nil || err.Error() != "stage_generation_mismatch" {
		t.Fatalf("wrong generation markStreamedStageCommitted err = %v, want generation mismatch", err)
	}
	if err := svc.markStreamedStageCommitted("xfer-lease", gen); err != nil {
		t.Fatalf("matching markStreamedStageCommitted: %v", err)
	}
}

func TestPrepareAndCommitRejectWhileStreamLeaseActive(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	svc, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-active", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	defer svc.CancelStreamedStage("xfer-active", gen, "test_done")

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

func TestStreamedStageDiagHookClearsOnCommittedStage(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	svc, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-hook-commit", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if !abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook inactive after BeginStreamedStage")
	}
	if err := svc.markStreamedStageCommitted("xfer-hook-commit", gen); err != nil {
		t.Fatalf("markStreamedStageCommitted: %v", err)
	}
	clearABUpdateDiagHook()
	if abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook still active after committed stage")
	}
}

func TestStreamedStageDiagHookClearIsLeaseScoped(t *testing.T) {
	clearABUpdateDiagHook()
	installABUpdateDiagHook("current-xfer", 2)
	clearABUpdateDiagHookFor("stale-xfer", 1)
	if !abupdateDiagHookActiveForTest() {
		t.Fatal("stale generation cleared current diagnostic hook")
	}
	clearABUpdateDiagHookFor("current-xfer", 2)
	if abupdateDiagHookActiveForTest() {
		t.Fatal("matching generation did not clear diagnostic hook")
	}
}

func TestStreamedStageDiagHookClearsOnAbort(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	svc, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-hook-abort", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if !abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook inactive after BeginStreamedStage")
	}
	svc.AbortStreamedStage("xfer-hook-abort", gen, "test_abort")
	if abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook still active after abort")
	}
}

func TestStreamedStageDiagHookClearsOnCommitError(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	svc, cancel := runService(t, b, Options{Conn: conn})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-hook-commit-error", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if !abupdateDiagHookActiveForTest() {
		t.Fatal("diagnostic hook inactive after BeginStreamedStage")
	}
	if _, err := svc.CommitStreamedStage("xfer-hook-commit-error", gen); err == nil {
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

	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-cancel", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if err := svc.markStreamedStageCommitted("xfer-cancel", gen); err != nil {
		t.Fatalf("markStreamedStageCommitted: %v", err)
	}
	svc.CancelStreamedStage("xfer-cancel", gen, "test_cancel")

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

	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Applier:       app,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-released", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if err := svc.WriteStreamedStage("xfer-released", gen, []byte("blob")); err != nil {
		t.Fatalf("WriteStreamedStage: %v", err)
	}
	if _, err := svc.CommitStreamedStage("xfer-released", gen); err != nil {
		t.Fatalf("CommitStreamedStage: %v", err)
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

	svc.CancelStreamedStage("xfer-released", gen, "late_cancel")
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

	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-current", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	defer svc.CancelStreamedStage("xfer-current", gen, "test_done")

	if err := svc.WriteStreamedStage("wrong-xfer", gen, []byte("data")); err == nil || err.Error() != "stage_generation_mismatch" {
		t.Fatalf("wrong xfer WriteStreamedStage err = %v, want generation mismatch", err)
	}
	if _, err := svc.CommitStreamedStage("xfer-current", gen+1); err == nil || err.Error() != "stage_generation_mismatch" {
		t.Fatalf("stale generation CommitStreamedStage err = %v, want generation mismatch", err)
	}
	if err := svc.WriteStreamedStage("xfer-current", gen, []byte("data")); err != nil {
		t.Fatalf("WriteStreamedStage: %v", err)
	}
	if _, err := svc.CommitStreamedStage("xfer-current", gen); err != nil {
		t.Fatalf("CommitStreamedStage: %v", err)
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

	payload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	reply, ok := payload.(Reply)
	if !ok {
		t.Fatalf("reply payload type = %T", payload)
	}
	if reply.OK {
		t.Fatalf("commit unexpectedly OK without staged image: %+v", reply)
	}
	if reply.Error != ErrNoStagedImage {
		t.Fatalf("commit error = %q, want %q", reply.Error, ErrNoStagedImage)
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

	payload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	reply, _ := payload.(Reply)
	if reply.OK || reply.Error != ErrNoStagedImage {
		t.Fatalf("commit reply = %+v, want refusal=no_staged_image", reply)
	}
}

func TestCommitUsesPreparedExpectedImageOverCommitPayload(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")

	memMD := NewMemoryMetadata()
	if err := memMD.WriteStagedDescriptor(StagedDescriptor{
		Version:       "9.9.9",
		BuildID:       "build-b",
		ImageID:       "image-B",
		Length:        4096,
		Slot:          1,
		PayloadSHA256: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatalf("write staged descriptor: %v", err)
	}
	app := &fakeApplier{rebootCh: make(chan StagedDescriptor, 1)}
	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Metadata:      memMD,
		MetadataWrite: memMD,
		Applier:       app,
	})
	defer cancel()

	svc.setJobContext("job-image-a", "image-A")
	svc.transitionTo(StateStaged, "", "9.9.9")

	payload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{ExpectedImageID: "image-B"})
	reply, ok := payload.(Reply)
	if !ok || reply.OK || reply.Error != ErrImageIDMismatch {
		t.Fatalf("commit reply = %#v, want image id mismatch", payload)
	}
	canCalls, rebootCalls := app.callCounts()
	if canCalls != 0 || rebootCalls != 0 {
		t.Fatalf("applier called despite target mismatch: can=%d reboot=%d", canCalls, rebootCalls)
	}
	select {
	case d := <-app.rebootCh:
		t.Fatalf("unexpected reboot descriptor: %+v", d)
	default:
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
	_ = requestUpdaterReply(t, caller, TopicStageRPC, preparedStagePayload(t, caller, svc, "xfer-x", []byte("blob")))

	payload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	reply, _ := payload.(Reply)
	if reply.OK || reply.Error != ErrApplyUnavailable {
		t.Fatalf("commit reply = %+v, want refusal=commit_failed", reply)
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
	_ = requestUpdaterReply(t, caller, TopicStageRPC, preparedStagePayload(t, caller, svc, "x", []byte("blob")))

	// Commit.
	payload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	reply, _ := payload.(CommitReply)
	if !reply.Accepted || !reply.RebootRequired {
		t.Fatalf("commit reply = %+v", reply)
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

	_ = requestUpdaterReply(t, caller, TopicStageRPC, preparedStagePayload(t, caller, svc, "x", []byte("blob")))

	payload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	reply, _ := payload.(CommitReply)
	if !reply.Accepted || !reply.RebootRequired {
		t.Fatalf("commit reply = %+v", reply)
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

	_, generation := preparedStreamedStageLease(t, caller, svc, "xfer-1", []byte("blob"))
	if _, err := svc.CommitStreamedStage("xfer-1", generation); err == nil || !strings.Contains(err.Error(), "verifier_stub") {
		t.Fatalf("commit streamed stage err = %v, want stub sentinel", err)
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
	target := signedimage.Target{
		ProductFamily:   "bigbox",
		HardwareProfile: "bb-v1-cm5-2",
		MCUBoardFamily:  "rp2354a",
	}
	artefact, _, err := signedimage.Pack([]byte("signed payload"), signedimage.PackOptions{
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

	payload := requestUpdaterReply(t, caller, TopicStageRPC, preparedStagePayload(t, caller, svc, "signed-xfer", artefact))
	reply, _ := payload.(StageReply)
	if !reply.OK {
		t.Fatalf("stage reply not ok: %+v", reply)
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
	payload := requestUpdaterReply(t, caller, TopicStageRPC, preparedStagePayload(t, caller, svc, "xfer-w11", []byte("blob")))
	reply, _ := payload.(StageReply)
	if !reply.OK {
		t.Fatalf("stage reply not ok: %+v", reply)
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
	commitPayload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	commitReply, _ := commitPayload.(CommitReply)
	if !commitReply.Accepted {
		t.Fatalf("commit reply not ok: %+v", commitReply)
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateRebooting })
	if strValue(up.PendingVersion) != "9.9.9" {
		t.Fatalf("pending_version = %q, want 9.9.9", strValue(up.PendingVersion))
	}
}

func TestStageFailureClearsStaleStagedDescriptor(t *testing.T) {
	// A (stage A) -> (prepare for B) -> (stage B fails) flow must not leave
	// descriptor A persisted. The next commit should return no_staged_image
	// rather than committing stale firmware.
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	upSub := caller.Subscribe(TopicUpdaterFact)
	defer caller.Unsubscribe(upSub)

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

	// Drive updater/main streamed staging to verifier failure.
	_, generation := preparedStreamedStageLease(t, caller, svc, "x", []byte("blob"))
	if _, err := svc.CommitStreamedStage("x", generation); err == nil || err.Error() != "bad_signature" {
		t.Fatalf("commit streamed stage err = %v, want bad_signature", err)
	}
	_ = waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })

	// The stale descriptor must have been cleared.
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatalf("stale staged descriptor survived receiver failure")
	}

	// Commit must refuse with no_staged_image rather than commit the
	// stale image.
	commitPayload := requestUpdaterReply(t, caller, TopicCommitRPC, CommitRequest{})
	reply, _ := commitPayload.(Reply)
	if reply.OK || reply.Error != ErrNoStagedImage {
		t.Fatalf("commit reply = %+v, want refusal=no_staged_image", reply)
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

	payload := requestUpdaterReply(t, caller, TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU})
	reply, _ := payload.(PrepareReply)
	if !reply.Ready {
		t.Fatalf("prepare reply = %+v", reply)
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

	payload := requestUpdaterReply(t, caller, TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU})
	reply, ok := payload.(Reply)
	if !ok {
		t.Fatalf("reply payload type = %T", payload)
	}
	if reply.OK || reply.Error != "metadata_clear_failed:flash_busy" {
		t.Fatalf("prepare reply = %+v, want metadata clear failure", reply)
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if strValue(up.LastError) != "metadata_clear_failed:flash_busy" {
		t.Fatalf("last_error = %q, want metadata clear failure", strValue(up.LastError))
	}

	failMD.err = nil
	retryPayload := requestUpdaterReply(t, caller, TopicPrepareRPC, PrepareRequest{Target: PrepareTargetMCU})
	retryReply, ok := retryPayload.(PrepareReply)
	if !ok {
		t.Fatalf("retry reply payload type = %T", retryPayload)
	}
	if !retryReply.Ready {
		t.Fatalf("retry prepare reply = %+v, want ready", retryReply)
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

	payload := requestUpdaterReply(t, caller, TopicStageRPC, preparedStagePayload(t, caller, svc, "xfer-2", []byte("blob")))
	reply, ok := payload.(StageReply)
	if !ok || !reply.OK || reply.Stage != "staged" {
		t.Fatalf("stage reply = %+v ok-type=%v", reply, ok)
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

	_, generation := preparedStreamedStageLease(t, caller, svc, "xfer-3", []byte("blob"))
	if _, err := svc.CommitStreamedStage("xfer-3", generation); err == nil || err.Error() != "manifest_check_failed" {
		t.Fatalf("commit streamed stage err = %v, want manifest_check_failed", err)
	}

	up := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if strValue(up.LastError) != "manifest_check_failed" {
		t.Fatalf("last_error = %q, want manifest_check_failed", strValue(up.LastError))
	}
}

func TestCriticalFactsDedupOnUpdaterStateChange(t *testing.T) {
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
	up := waitForFact[UpdaterFact](t, upSub, nil)
	if up.State != StateReady {
		t.Fatalf("updater state = %q, want %q", up.State, StateReady)
	}
	assertNoFact(t, swSub, 20*time.Millisecond, "software")
	assertNoFact(t, hSub, 20*time.Millisecond, "health")
}

func TestCriticalFactsDedupUntilLiveness(t *testing.T) {
	b := bus.NewBus(32, "+", "#")
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	swSub := observer.Subscribe(TopicSoftwareFact)
	defer observer.Unsubscribe(swSub)
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	hSub := observer.Subscribe(TopicHealthFact)
	defer observer.Unsubscribe(hSub)

	svc := New(Options{
		Conn: conn,
		Identity: Identity{
			Version: "0.0.0-test",
			Build:   "build-test",
			ImageID: "img-test",
		},
		CriticalFacts: CriticalFactConfig{
			LivenessInterval: 25 * time.Millisecond,
		},
	})

	svc.PublishCriticalFacts()
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)

	svc.PublishCriticalFacts()
	assertNoFact(t, swSub, 10*time.Millisecond, "software")
	assertNoFact(t, upSub, 10*time.Millisecond, "updater")
	assertNoFact(t, hSub, 10*time.Millisecond, "health")

	time.Sleep(30 * time.Millisecond)
	svc.PublishCriticalFacts()
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)
}

func TestCriticalFactLivenessCadence(t *testing.T) {
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
		Conn:          conn,
		CriticalFacts: testCriticalFactConfig(),
	})
	defer cancel()
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)

	assertNoFact(t, swSub, 15*time.Millisecond, "software")
	_, _, _ = waitForCriticalFactSet(t, swSub, upSub, hSub)
}

func TestFabricLinkStateDoesNotTriggerUpdaterFacts(t *testing.T) {
	b := bus.NewBus(64, "+", "#")
	conn := b.NewConnection("updater")
	observer := b.NewConnection("observer")
	swSub := observer.Subscribe(TopicSoftwareFact)
	defer observer.Unsubscribe(swSub)

	_, cancel := runService(t, b, Options{
		Conn:          conn,
		CriticalFacts: CriticalFactConfig{LivenessInterval: 100 * time.Millisecond},
	})
	defer cancel()
	_ = waitForFact[SoftwareFact](t, swSub, nil)
	drainSubscription(swSub)

	fabric := b.NewConnection("fabric-test")
	fabric.Publish(fabric.NewMessage(
		bus.T("state", "fabric", "link", "mcu-uart0"),
		map[string]any{"ready": true, "established": true, "peer_sid": "cm5-a", "local_sid": "mcu-a"},
		true,
	))

	assertNoSoftwareRepublish(t, swSub, 40*time.Millisecond)
}

func assertNoFact(t *testing.T, sub *bus.Subscription, d time.Duration, label string) {
	t.Helper()
	settled := time.After(d)
	for {
		select {
		case <-sub.Channel():
			t.Fatalf("unexpected %s fact republish", label)
		case <-settled:
			return
		}
	}
}

func assertNoSoftwareRepublish(t *testing.T, sub *bus.Subscription, d time.Duration) {
	t.Helper()
	assertNoFact(t, sub, d, "software")
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

func TestDuplicateAcceptedCommitRequiresMatchingNonEmptyToken(t *testing.T) {
	svc := &Service{
		state:                     StateRebooting,
		lastCommitJobID:           "job-1",
		lastCommitExpectedImageID: "mcu-dev-1",
		lastCommitImageID:         "mcu-dev-1",
	}

	if svc.duplicateAcceptedCommitLocked(CommitRequest{JobID: "job-1", ExpectedImageID: "mcu-dev-1"}) {
		t.Fatal("duplicate commit without remembered token was accepted")
	}

	svc.lastCommitToken = "commit-token-1"
	if svc.duplicateAcceptedCommitLocked(CommitRequest{JobID: "job-1", ExpectedImageID: "mcu-dev-1"}) {
		t.Fatal("duplicate commit without request token was accepted")
	}
	if svc.duplicateAcceptedCommitLocked(CommitRequest{JobID: "job-1", ExpectedImageID: "mcu-dev-1", CommitToken: "other-token"}) {
		t.Fatal("duplicate commit with mismatched token was accepted")
	}
	if !svc.duplicateAcceptedCommitLocked(CommitRequest{JobID: "job-1", ExpectedImageID: "mcu-dev-1", CommitToken: "commit-token-1"}) {
		t.Fatal("duplicate commit with matching token was not accepted")
	}
}

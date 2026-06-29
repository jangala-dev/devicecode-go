package updater

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/retainedpub"
)

// Local-bus topics the updater binds to. Fabric routes wire
// prepare-update/commit-update calls here. The staging path is a local RPC
// called by fabric after xfer_commit for
// target="updater/main"; raw/member topic names are not wire contract.
var (
	TopicPrepareRPC = bus.T("rpc", "updater", "prepare")
	TopicCommitRPC  = bus.T("rpc", "updater", "commit")
	TopicStageRPC   = bus.T("rpc", "updater", "stage")

	TopicSoftwareFact = bus.T("state", "self", "software")
	TopicUpdaterFact  = bus.T("state", "self", "updater")
	TopicHealthFact   = bus.T("state", "self", "health")
)

// Identity carries the build-time stamp the software fact publishes.
// Filled in main.go (or tests) when constructing the updater.
type Identity struct {
	Version string
	Build   string
	ImageID string
}

// MetadataReader is the read side of the abupdate metadata block — the
// updater pulls payload_sha256 and the staged descriptor (if any) from
// here at boot. The matching MetadataWriter handles staging-side
// persistence.
type MetadataReader interface {
	PayloadSHA256() string
	StagedDescriptor() (StagedDescriptor, bool)
}

// MetadataWriter is the write side: updater/main staging hands a verified
// StagedDescriptor + payload_sha256 here so the next boot's
// MetadataReader observes them. A default in-memory implementation is
// supplied for host tests and non-persistent builds; flash-backed
// implementations may persist this in the abupdate metadata block.
type MetadataWriter interface {
	WriteStagedDescriptor(d StagedDescriptor) error
	ClearStagedDescriptor() error
}

// MemoryMetadata is the default in-memory MetadataReader+Writer used by host
// tests and non-persistent builds.
//
// Two separate payload-hash fields are intentional:
//   - runningPayloadSHA — the hash of the IMAGE THAT IS RUNNING. Set
//     once at boot from the active slot's metadata block. Read by
//     SoftwareFact.PayloadSHA256.
//   - stagedPayloadSHA — carried inside StagedDescriptor; lives only
//     when a staged image is present. Cleared by
//     ClearStagedDescriptor; never bleeds into the running fact.
//
// Sharing a single field would let prepare/stage-failure leave a
// stale staged hash sitting on the wire-visible software fact even
// after the descriptor was cleared.
type MemoryMetadata struct {
	mu                sync.Mutex
	runningPayloadSHA string
	desc              StagedDescriptor
	hasDesc           bool
}

// NewMemoryMetadata returns an empty MemoryMetadata. runningPayloadSHA
// stays "" until the caller calls SetRunningPayloadSHA from the boot
// path (typically reading the active slot's metadata block); the
// staged descriptor stays empty until updater/main staging writes it.
func NewMemoryMetadata() *MemoryMetadata { return &MemoryMetadata{} }

// SetRunningPayloadSHA records the hash of the currently-running
// image. Hardware builds can source this from the active slot's flash
// metadata at boot; tests can call it directly. Bare 64-char lower-hex
// per the spec.
func (m *MemoryMetadata) SetRunningPayloadSHA(sha string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runningPayloadSHA = sha
}

func (m *MemoryMetadata) PayloadSHA256() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningPayloadSHA
}

func (m *MemoryMetadata) StagedDescriptor() (StagedDescriptor, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.desc, m.hasDesc
}

func (m *MemoryMetadata) WriteStagedDescriptor(d StagedDescriptor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desc = d
	m.hasDesc = true
	// Note: running hash is NOT updated here. The staged hash lives
	// inside the descriptor; it only becomes the running hash after
	// a successful boot into the staged slot, at which point the
	// next boot's SetRunningPayloadSHA pulls it from flash metadata.
	return nil
}

func (m *MemoryMetadata) ClearStagedDescriptor() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desc = StagedDescriptor{}
	m.hasDesc = false
	return nil
}

// nullMetadata is the zero-value default when the caller doesn't
// provide a MetadataReader. Read-only — no Write methods.
type nullMetadata struct{}

func (nullMetadata) PayloadSHA256() string                      { return "" }
func (nullMetadata) StagedDescriptor() (StagedDescriptor, bool) { return StagedDescriptor{}, false }

type applyRebootResult struct {
	desc StagedDescriptor
	err  error
}

// Service is the updater state machine + RPC binder. Constructed once
// in reactor.go and run in its own goroutine.
type Service struct {
	conn          *bus.Connection
	verifier      Verifier
	applier       Applier
	identity      Identity
	metadata      MetadataReader
	metadataWrite MetadataWriter

	mu                        sync.Mutex
	state                     State
	lastError                 string
	pendingVersion            string
	pendingImageID            string
	stagedImageID             string
	jobID                     string
	preparing                 bool
	lastCommitJobID           string
	lastCommitExpectedImageID string
	lastCommitImageID         string
	lastCommitToken           string
	bootBuyRC                 int32

	stageGeneration     uint64
	streamLeaseActive   bool
	streamXferID        string
	streamCancelled     bool
	streamCommitted     bool
	streamStageResult   streamedStage
	streamStageResultOK bool

	stageCommands       chan streamedStageCommand
	stageReplyPool      chan chan streamedStageCommandResult
	stageWorkerCommands chan streamedStageWorkerCommand
	stageWorkerResults  chan streamedStageWorkerResult
	pendingStageCommand *streamedStageCommand
	stageReady          chan struct{}
	stageStopped        chan struct{}
	stageReadyOnce      sync.Once
	stageStoppedOnce    sync.Once

	applyResults chan applyRebootResult

	criticalFacts CriticalFactConfig

	softwarePub retainedpub.Publisher[SoftwareFact]
	updaterPub  retainedpub.Publisher[UpdaterFact]
	healthPub   retainedpub.Publisher[HealthFact]
}

// CriticalFactConfig controls liveness publication of the MCU critical
// retained facts. The updater does not watch Fabric link state; Fabric is
// responsible for replaying retained facts when a peer establishes a session.
//
// Content changes publish immediately. Unchanged facts republish only at
// LivenessInterval so observers can tell the updater is still alive.
type CriticalFactConfig struct {
	LivenessInterval time.Duration
}

const defaultCriticalFactLivenessInterval = 15 * time.Second

func normalizeCriticalFactConfig(cfg CriticalFactConfig) CriticalFactConfig {
	if cfg.LivenessInterval <= 0 {
		cfg.LivenessInterval = defaultCriticalFactLivenessInterval
	}
	return cfg
}

// Options bundle the constructor parameters so Service can grow new
// dependencies without churning callers.
type Options struct {
	Conn          *bus.Connection
	Verifier      Verifier
	Applier       Applier
	Identity      Identity
	Metadata      MetadataReader
	MetadataWrite MetadataWriter
	BootBuyRC     int32
	CriticalFacts CriticalFactConfig
}

// New builds a Service. Verifier defaults to the rejecting StubVerifier
// and Applier defaults to RefusingApplier so the production wiring
// never claims an apply succeeded when the apply path isn't
// implemented yet. Metadata defaults to a fresh in-memory
// implementation that's both reader and writer — fine for tests and
// for the rejecting-stub production path where nothing ever writes
// anyway.
func New(opts Options) *Service {
	v := opts.Verifier
	if v == nil {
		v = StubVerifier()
	}
	a := opts.Applier
	if a == nil {
		a = RefusingApplier()
	}
	mr := opts.Metadata
	mw := opts.MetadataWrite
	if mr == nil && mw == nil {
		shared := NewMemoryMetadata()
		mr = shared
		mw = shared
	} else if mr == nil {
		mr = nullMetadata{}
	} else if mw == nil {
		// Reader-only: writes from staging become no-ops.
		mw = noopMetadataWriter{}
	}
	s := &Service{
		conn:                opts.Conn,
		verifier:            v,
		applier:             a,
		identity:            opts.Identity,
		metadata:            mr,
		metadataWrite:       mw,
		state:               StateRunning,
		bootBuyRC:           opts.BootBuyRC,
		stageCommands:       make(chan streamedStageCommand, 1),
		stageReplyPool:      make(chan chan streamedStageCommandResult, 2),
		stageWorkerCommands: make(chan streamedStageWorkerCommand, 1),
		stageWorkerResults:  make(chan streamedStageWorkerResult, 1),
		stageReady:          make(chan struct{}),
		stageStopped:        make(chan struct{}),
		applyResults:        make(chan applyRebootResult, 1),
		criticalFacts:       normalizeCriticalFactConfig(opts.CriticalFacts),
	}
	s.softwarePub = retainedpub.New(opts.Conn, TopicSoftwareFact, retainedpub.ComparableEqual[SoftwareFact])
	s.updaterPub = retainedpub.New(opts.Conn, TopicUpdaterFact, updaterFactsEqual)
	s.stageReplyPool <- make(chan streamedStageCommandResult, 1)
	s.stageReplyPool <- make(chan streamedStageCommandResult, 1)
	s.healthPub = retainedpub.New(opts.Conn, TopicHealthFact, retainedpub.ComparableEqual[HealthFact])
	if opts.BootBuyRC != 0 {
		s.state = StateFailed
		s.lastError = ErrABUpdateBuyFailed
	}
	return s
}

// noopMetadataWriter is the writer-side fallback when the caller
// supplied a MetadataReader without a matching writer.
type noopMetadataWriter struct{}

func (noopMetadataWriter) WriteStagedDescriptor(d StagedDescriptor) error {
	return nil
}
func (noopMetadataWriter) ClearStagedDescriptor() error {
	return nil
}

// Run binds the RPC + staging topics, publishes the initial fact surface,
// and runs the critical-fact liveness cadence. Blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	s.stageReadyOnce.Do(func() { close(s.stageReady) })
	go s.runStreamedStageWorker(ctx)
	defer s.stageStoppedOnce.Do(func() { close(s.stageStopped) })

	prepareBinding := s.conn.Bind(TopicPrepareRPC, func(ctx context.Context, payload any) (any, error) {
		return s.prepare(payload), nil
	})
	defer prepareBinding.Close()

	commitBinding := s.conn.Bind(TopicCommitRPC, func(ctx context.Context, payload any) (any, error) {
		return s.commit(payload), nil
	})
	defer commitBinding.Close()

	stageBinding := s.conn.Bind(TopicStageRPC, func(ctx context.Context, payload any) (any, error) {
		return s.stage(payload), nil
	})
	defer stageBinding.Close()

	// Initial fact publish: tells local observers we're alive and reports
	// build identity + the freshly generated boot_id. Fabric replays the
	// retained state/self/* surface to a CM5 peer when a session establishes.
	s.PublishCriticalFacts()

	criticalTimer := time.NewTimer(s.criticalFactLivenessInterval())
	defer func() {
		if !criticalTimer.Stop() {
			select {
			case <-criticalTimer.C:
			default:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-s.stageCommands:
			s.handleStreamedStageCommand(cmd)
		case result := <-s.stageWorkerResults:
			s.handleStreamedStageWorkerResult(result)
		case result := <-s.applyResults:
			s.failRebootIfCurrent(result.desc, result.err)
		case <-criticalTimer.C:
			s.PublishCriticalFacts()
			criticalTimer.Reset(s.criticalFactLivenessInterval())
		}
	}
}

// PublishCriticalFacts publishes the retained state/self facts that CM5 update
// reconcile treats as mandatory for the MCU component. Unchanged facts are
// suppressed until their liveness interval elapses.
func (s *Service) PublishCriticalFacts() {
	s.PublishSoftware()
	s.PublishUpdater()
	s.PublishHealth("ok", "")
}

// transitionTo updates state under the lock and publishes the updater
// fact. Returns the previous state for callers that want to log or
// confirm a precondition.
func (s *Service) transitionTo(next State, lastError, pendingVersion string) State {
	s.mu.Lock()
	prev := s.state
	s.state = next
	s.bootBuyRC = 0
	if lastError != "" || (next != StateFailed && next != StateRollbackDetected) {
		s.lastError = lastError
	}
	if pendingVersion != "" {
		s.pendingVersion = pendingVersion
	} else if next == StatePreparing || next == StateReady || next == StateReceiving {
		s.pendingVersion = ""
	}
	s.mu.Unlock()
	s.PublishCriticalFacts()
	return prev
}

func (s *Service) failRebootIfCurrent(desc StagedDescriptor, err error) bool {
	if err == nil {
		return false
	}
	s.mu.Lock()
	matches := s.state == StateRebooting &&
		s.pendingVersion == desc.Version &&
		s.stagedImageID == desc.ImageID
	s.mu.Unlock()
	if !matches {
		return false
	}
	s.transitionTo(StateFailed, err.Error(), desc.Version)
	return true
}

func (s *Service) setJobContext(jobID, pendingImageID string) {
	s.mu.Lock()
	s.jobID = jobID
	s.pendingImageID = pendingImageID
	s.stagedImageID = ""
	s.mu.Unlock()
}

func (s *Service) setStagedImage(imageID, version string) {
	s.mu.Lock()
	s.stagedImageID = imageID
	if version != "" {
		s.pendingVersion = version
	}
	s.mu.Unlock()
}

func (s *Service) clearStagedImage() {
	s.mu.Lock()
	s.stagedImageID = ""
	s.pendingVersion = ""
	s.mu.Unlock()
}

// markPrepareDone clears the preparing flag and guards prepare re-entry.
func (s *Service) markPrepareDone() {
	s.mu.Lock()
	s.preparing = false
	s.mu.Unlock()
}

// boot-time initialization helper — main.go calls this before opening
// fabric so the first software-fact publish has a non-empty boot_id.
func (s *Service) ensureBootID() string {
	id := BootID()
	if id == "" {
		id = GenerateBootID()
	}
	return id
}

// jsonDecode is a small helper that tolerates both already-typed
// payloads (Go-side test wiring) and raw JSON payloads (real wire).
// Returns the decoded value or false on a hopeless mismatch.
func jsonDecode[T any](payload any) (T, bool) {
	var out T
	switch v := payload.(type) {
	case nil:
		return out, true
	case T:
		return v, true
	case json.RawMessage:
		if len(v) == 0 {
			return out, true
		}
		if err := json.Unmarshal(v, &out); err != nil {
			return out, false
		}
		return out, true
	case []byte:
		if len(v) == 0 {
			return out, true
		}
		if err := json.Unmarshal(v, &out); err != nil {
			return out, false
		}
		return out, true
	}
	// Fall back to re-marshaling unknown shapes; covers the test path
	// where callers pass map[string]any that JSON-roundtrips.
	b, err := json.Marshal(payload)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, false
	}
	return out, true
}

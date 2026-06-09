package updater

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/otadiag"
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

	// TopicFabricLink is the wildcard the updater watches to drive the
	// post-hello_ack republish. The fabric session retains a
	// payload at state/fabric/link/<link_id> on every link-state edge;
	// we pick out Ready-true transitions and call PublishCriticalFacts()
	// so the CM5 sees fresh state/self/* facts on every newly established
	// session, warm or cold.
	TopicFabricLink = bus.T("state", "fabric", "link", "+")
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

	mu             sync.Mutex
	state          State
	lastError      string
	pendingVersion string
	pendingImageID string
	stagedImageID  string
	jobID          string
	preparing      bool
	bootBuyRC      int32

	stageGeneration     uint64
	streamLeaseActive   bool
	streamXferID        string
	streamCancelled     bool
	streamCommitted     bool
	streamStageResult   streamedStage
	streamStageResultOK bool

	stageCommands       chan streamedStageCommand
	stageWorkerCommands chan streamedStageWorkerCommand
	stageWorkerResults  chan streamedStageWorkerResult
	pendingStageCommand *streamedStageCommand
	stageReady          chan struct{}
	stageStopped        chan struct{}
	stageReadyOnce      sync.Once
	stageStoppedOnce    sync.Once

	applyResults chan applyRebootResult

	criticalRepublish CriticalRepublishConfig

	// Logger seam — left as a small helper so tests can plug in. nil in
	// tests means stderr-style println.
	logf func(string, ...any)
}

// CriticalRepublishConfig controls level-triggered publication of the MCU
// critical facts while a Fabric peer remains ready. Zero values use the
// hardware-safe defaults.
type CriticalRepublishConfig struct {
	BurstInterval  time.Duration
	BurstDuration  time.Duration
	SteadyInterval time.Duration
}

const (
	defaultCriticalRepublishBurstInterval  = time.Second
	defaultCriticalRepublishBurstDuration  = 10 * time.Second
	defaultCriticalRepublishSteadyInterval = 15 * time.Second
)

func normalizeCriticalRepublishConfig(cfg CriticalRepublishConfig) CriticalRepublishConfig {
	if cfg.BurstInterval <= 0 {
		cfg.BurstInterval = defaultCriticalRepublishBurstInterval
	}
	if cfg.BurstDuration <= 0 {
		cfg.BurstDuration = defaultCriticalRepublishBurstDuration
	}
	if cfg.SteadyInterval <= 0 {
		cfg.SteadyInterval = defaultCriticalRepublishSteadyInterval
	}
	return cfg
}

// Options bundle the constructor parameters so Service can grow new
// dependencies without churning callers.
type Options struct {
	Conn              *bus.Connection
	Verifier          Verifier
	Applier           Applier
	Identity          Identity
	Metadata          MetadataReader
	MetadataWrite     MetadataWriter
	BootBuyRC         int32
	CriticalRepublish CriticalRepublishConfig
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
		stageWorkerCommands: make(chan streamedStageWorkerCommand, 1),
		stageWorkerResults:  make(chan streamedStageWorkerResult, 1),
		stageReady:          make(chan struct{}),
		stageStopped:        make(chan struct{}),
		applyResults:        make(chan applyRebootResult, 1),
		criticalRepublish: normalizeCriticalRepublishConfig(
			opts.CriticalRepublish,
		),
	}
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

// Run binds the RPC + staging topics, publishes the initial fact
// surface, and watches the fabric link-state retain for ready-true
// edges. Blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	s.stageReadyOnce.Do(func() { close(s.stageReady) })
	go s.runStreamedStageWorker(ctx)
	defer s.stageStoppedOnce.Do(func() { close(s.stageStopped) })
	defer otadiag.StopUpdateWindow("updater_stop")

	prepareSub := s.conn.Subscribe(TopicPrepareRPC)
	defer s.conn.Unsubscribe(prepareSub)

	commitSub := s.conn.Subscribe(TopicCommitRPC)
	defer s.conn.Unsubscribe(commitSub)

	stageSub := s.conn.Subscribe(TopicStageRPC)
	defer s.conn.Unsubscribe(stageSub)

	linkSub := s.conn.Subscribe(TopicFabricLink)
	defer s.conn.Unsubscribe(linkSub)

	// Initial fact publish: tells the CM5 we're alive and reports
	// build identity + the freshly generated boot_id.
	s.PublishCriticalFacts()

	// Track per-link ready/session identity so a CM5 SID change republishes
	// critical retained facts even if the link does not emit a clean false edge.
	linkState := map[string]linkObservation{}
	var criticalTimer *time.Timer
	var criticalTimerC <-chan time.Time
	var burstUntil time.Time
	stopCriticalTimer := func() {
		if criticalTimer == nil {
			return
		}
		if !criticalTimer.Stop() {
			select {
			case <-criticalTimer.C:
			default:
			}
		}
		criticalTimerC = nil
	}
	defer stopCriticalTimer()
	armCriticalTimer := func(delay time.Duration) {
		if delay <= 0 {
			delay = s.criticalRepublish.SteadyInterval
		}
		if criticalTimer == nil {
			criticalTimer = time.NewTimer(delay)
		} else {
			if !criticalTimer.Stop() {
				select {
				case <-criticalTimer.C:
				default:
				}
			}
			criticalTimer.Reset(delay)
		}
		criticalTimerC = criticalTimer.C
	}
	anyReadyLink := func() bool {
		for _, obs := range linkState {
			if obs.Ready {
				return true
			}
		}
		return false
	}
	startCriticalCadence := func(now time.Time) {
		burstUntil = now.Add(s.criticalRepublish.BurstDuration)
		armCriticalTimer(s.criticalRepublish.BurstInterval)
	}
	runCriticalCadence := func(now time.Time) {
		if !anyReadyLink() {
			burstUntil = time.Time{}
			stopCriticalTimer()
			return
		}
		if !burstUntil.IsZero() && now.Before(burstUntil) {
			s.PublishCriticalFacts()
			armCriticalTimer(s.criticalRepublish.BurstInterval)
			return
		}
		burstUntil = time.Time{}
		s.PublishCriticalFacts()
		armCriticalTimer(s.criticalRepublish.SteadyInterval)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-prepareSub.Channel():
			if !ok || msg == nil {
				continue
			}
			s.handlePrepare(msg)
		case msg, ok := <-commitSub.Channel():
			if !ok || msg == nil {
				continue
			}
			s.handleCommit(msg)
		case msg, ok := <-stageSub.Channel():
			if !ok || msg == nil {
				continue
			}
			s.handleStage(msg)
		case cmd := <-s.stageCommands:
			s.handleStreamedStageCommand(cmd)
		case result := <-s.stageWorkerResults:
			s.handleStreamedStageWorkerResult(result)
		case result := <-s.applyResults:
			s.failRebootIfCurrent(result.desc, result.err)
		case now := <-criticalTimerC:
			runCriticalCadence(now)
		case msg, ok := <-linkSub.Channel():
			if !ok || msg == nil {
				continue
			}
			linkID, obs := decodeLinkState(msg)
			if linkID == "" {
				continue
			}
			prev, hadPrev := linkState[linkID]
			if reason := republishReason(prev, obs, hadPrev); reason != "" {
				// Post-hello_ack republish. Mirrors the contract that state
				// facts are republished after every successful boot and on
				// every newly established session, warm or cold.
				s.logRepublish(reason, linkID, obs)
				s.PublishCriticalFacts()
				linkState[linkID] = obs
				startCriticalCadence(time.Now())
				continue
			}
			linkState[linkID] = obs
			if !obs.Ready && !anyReadyLink() {
				burstUntil = time.Time{}
				stopCriticalTimer()
			}
		}
	}
}

// PublishCriticalFacts re-emits the retained state/self facts that CM5 update
// reconcile treats as mandatory for the MCU component.
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

func (s *Service) diagSnapshotLocked() otadiag.StageSnapshot {
	xferID := s.streamXferID
	if xferID == "" {
		xferID = otadiag.XferNone
	}
	return otadiag.StageSnapshot{
		State:       string(s.state),
		Generation:  s.stageGeneration,
		LeaseActive: s.streamLeaseActive,
		XferID:      xferID,
	}
}

func setDiagSnapshot(snap otadiag.StageSnapshot) {
	otadiag.SetUpdaterSnapshot(snap)
}

func (s *Service) logRepublish(reason, linkID string, obs linkObservation) {
	if s.logf != nil {
		s.logf("updater republish reason=%s link=%s peer_sid=%s local_sid=%s", reason, linkID, obs.PeerSID, obs.LocalSID)
		return
	}
	println(
		"updater", "republish",
		"reason="+reason,
		"link="+linkID,
		"peer_sid="+obs.PeerSID,
		"local_sid="+obs.LocalSID,
	)
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

// markPrepareDone clears the preparing flag. handlePrepare/handleCommit
// guard re-entry through this.
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

// reply is a thin convenience wrapper that tolerates nil msg (defensive
// against bus quirks observed during fabric-protocol bring-up where a
// ctx cancel could land a nil message on the channel).
func (s *Service) reply(msg *bus.Message, payload any) {
	if msg == nil || !msg.CanReply() {
		return
	}
	s.conn.Reply(msg, payload, false)
}

type linkObservation struct {
	Ready    bool
	PeerSID  string
	LocalSID string
}

// fabricLinkObserver is implemented by services/fabric's retained link-state
// payload. Keeping this as a tiny structural interface avoids JSON reflection
// in the common in-process TinyGo path while still tolerating map/JSON payloads
// in host-side tests.
type fabricLinkObserver interface {
	FabricLinkObservation() (ready bool, peerSID string, localSID string)
}

func republishReason(prev, cur linkObservation, hadPrev bool) string {
	if !cur.Ready {
		return ""
	}
	if !hadPrev || !prev.Ready {
		return "ready_edge"
	}
	if prev.PeerSID != cur.PeerSID {
		return "peer_sid_changed"
	}
	if prev.LocalSID != cur.LocalSID {
		return "local_sid_changed"
	}
	return ""
}

// decodeLinkState extracts the link_id plus ready/session identity from a
// state/fabric/link/<id> retain. Tolerates both the typed payload
// shape published by services/fabric/session.go and a generic
// map[string]any (in-process test harnesses). Returns ("", zero)
// for any payload it can't make sense of — the caller treats that
// as "no edge".
func decodeLinkState(msg *bus.Message) (string, linkObservation) {
	var obs linkObservation
	if msg == nil {
		return "", obs
	}
	// Pull link_id from the topic tail (state/fabric/link/<id>).
	t := msg.Topic
	if t == nil || t.Len() < 4 {
		return "", obs
	}
	last := t.At(t.Len() - 1)
	linkID, _ := last.(string)
	if linkID == "" {
		return "", obs
	}
	switch p := msg.Payload.(type) {
	case nil:
		return linkID, obs
	case map[string]any:
		obs.Ready, _ = p["ready"].(bool)
		obs.PeerSID, _ = p["peer_sid"].(string)
		obs.LocalSID, _ = p["local_sid"].(string)
		return linkID, obs
	case fabricLinkObserver:
		obs.Ready, obs.PeerSID, obs.LocalSID = p.FabricLinkObservation()
		return linkID, obs
	}
	// Fall back to JSON probe for the typed-struct payload that
	// fabric publishes via its linkStatePayload type.
	b, err := json.Marshal(msg.Payload)
	if err != nil {
		return linkID, obs
	}
	var probe struct {
		Ready    bool   `json:"ready"`
		PeerSID  string `json:"peer_sid"`
		LocalSID string `json:"local_sid"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return linkID, obs
	}
	obs.Ready = probe.Ready
	obs.PeerSID = probe.PeerSID
	obs.LocalSID = probe.LocalSID
	return linkID, obs
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

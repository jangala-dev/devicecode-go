package updater

import (
	"context"
	"encoding/json"
	"sync"

	"devicecode-go/bus"
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
	// post-hello_ack republish (W10). The fabric session retains a
	// payload at state/fabric/link/<link_id> on every link-state edge;
	// we pick out Ready-true transitions and call Republish() so the
	// CM5 sees fresh state/self/* facts on every newly established
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
// here at boot. The fabric-update branch only requires reads from this
// interface; the matching MetadataWriter handles staging-side
// persistence in W11.
type MetadataReader interface {
	PayloadSHA256() string
	StagedDescriptor() (StagedDescriptor, bool)
}

// MetadataWriter is the write side: updater/main staging hands a verified
// StagedDescriptor + payload_sha256 here so the next boot's
// MetadataReader observes them. A default in-memory implementation is
// supplied (NewMemoryMetadata) for the fabric-update branch; the
// pico2-a-b/abupdate flash-backed implementation lands later (it
// touches the metadata sector at offset 0x000FF000 — see master
// plan §abupdate metadata block).
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
// image. fabric-security wires this from the active slot's flash
// metadata at boot; tests can call it directly. Bare 64-char
// lower-hex per the spec.
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

	applyResults chan applyRebootResult

	// Logger seam — left as a small helper so tests can plug in. nil in
	// tests means stderr-style println.
	logf func(string, ...any)
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
	return &Service{
		conn:          opts.Conn,
		verifier:      v,
		applier:       a,
		identity:      opts.Identity,
		metadata:      mr,
		metadataWrite: mw,
		state:         StateRunning,
		applyResults:  make(chan applyRebootResult, 1),
	}
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
// edges (W10). Blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
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
	s.PublishSoftware()
	s.PublishUpdater()
	s.PublishHealth("ok", "")

	// Track per-link ready state so we only republish on the
	// !Ready -> Ready edge, not on every retain churn.
	prevReady := map[string]bool{}

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
		case result := <-s.applyResults:
			s.failRebootIfCurrent(result.desc, result.err)
		case msg, ok := <-linkSub.Channel():
			if !ok || msg == nil {
				continue
			}
			linkID, ready := decodeLinkState(msg)
			if linkID == "" {
				continue
			}
			was := prevReady[linkID]
			if ready && !was {
				// W10: post-hello_ack republish. Mirrors the spec line
				// "republished after every successful boot AND on every
				// newly established session (hello_ack), warm or cold".
				s.Republish()
			}
			prevReady[linkID] = ready
		}
	}
}

// Republish re-emits all retained `state/self/*` facts. Wired up to
// fabric's session lifecycle so every new hello_ack triggers a fresh
// retain — required by the spec for warm-and-cold session resumes.
func (s *Service) Republish() {
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
	if lastError != "" || (next != StateFailed && next != StateRollbackDetected) {
		s.lastError = lastError
	}
	if pendingVersion != "" {
		s.pendingVersion = pendingVersion
	} else if next == StatePreparing || next == StateReady || next == StateReceiving {
		s.pendingVersion = ""
	}
	s.mu.Unlock()
	s.PublishUpdater()
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

// decodeLinkState extracts the link_id and ready flag from a
// state/fabric/link/<id> retain. Tolerates both the typed payload
// shape published by services/fabric/session.go and a generic
// map[string]any (in-process test harnesses). Returns ("", false)
// for any payload it can't make sense of — the caller treats that
// as "no edge".
func decodeLinkState(msg *bus.Message) (string, bool) {
	if msg == nil {
		return "", false
	}
	// Pull link_id from the topic tail (state/fabric/link/<id>).
	t := msg.Topic
	if t == nil || t.Len() < 4 {
		return "", false
	}
	last := t.At(t.Len() - 1)
	linkID, _ := last.(string)
	if linkID == "" {
		return "", false
	}
	switch p := msg.Payload.(type) {
	case nil:
		return linkID, false
	case map[string]any:
		ready, _ := p["ready"].(bool)
		return linkID, ready
	}
	// Fall back to JSON probe for the typed-struct payload that
	// fabric publishes via its linkStatePayload type.
	b, err := json.Marshal(msg.Payload)
	if err != nil {
		return linkID, false
	}
	var probe struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return linkID, false
	}
	return linkID, probe.Ready
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

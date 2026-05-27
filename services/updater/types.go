package updater

// State enumerates the canonical updater states from the current MCU
// contract. Empty string is accepted as the nil/unset state for local
// callers that have not published a fact yet.
type State string

const (
	StateRunning          State = "running"
	StateReady            State = "ready"
	StatePreparing        State = "preparing"
	StateReceiving        State = "receiving"
	StateStaged           State = "staged"
	StateCommitting       State = "committing"
	StateRebooting        State = "rebooting"
	StateFailed           State = "failed"
	StateRollbackDetected State = "rollback_detected"
)

func (s State) Allowed() bool {
	switch s {
	case "",
		StateRunning, StateReady, StatePreparing, StateReceiving,
		StateStaged, StateCommitting, StateRebooting,
		StateFailed, StateRollbackDetected:
		return true
	}
	return false
}

const (
	PrepareTargetMCU           = "mcu"
	TargetUpdaterMain          = "updater/main"
	DigestAlgXXHash32          = "xxhash32"
	DefaultMaxChunkSize uint32 = 2048
)

// PrepareRequest mirrors the current prepare-update payload.
// metadata is intentionally opaque so the CM5 can add fields without
// requiring a device firmware rebuild.
type PrepareRequest struct {
	JobID           string `json:"job_id,omitempty"`
	Target          string `json:"target,omitempty"`
	ExpectedImageID string `json:"expected_image_id,omitempty"`
	Metadata        any    `json:"metadata,omitempty"`
}

// CommitRequest mirrors commit-update.
type CommitRequest struct {
	JobID           string `json:"job_id,omitempty"`
	ExpectedImageID string `json:"expected_image_id,omitempty"`
	Metadata        any    `json:"metadata,omitempty"`
}

type PrepareReply struct {
	Ready        bool   `json:"ready"`
	Target       string `json:"target"`
	MaxChunkSize uint32 `json:"max_chunk_size"`
}

type CommitReply struct {
	Accepted       bool `json:"accepted"`
	RebootRequired bool `json:"reboot_required,omitempty"`
}

// Reply is retained for refusal/error replies. Successful prepare/commit
// calls use the contract-specific PrepareReply and CommitReply shapes.
type Reply struct {
	OK       bool   `json:"ok"`
	Accepted bool   `json:"accepted,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Refusal error strings — the Lua side compares against these.
const (
	ErrBusy           = "busy"
	ErrNothingStaged  = "nothing_staged"
	ErrTargetMismatch = "target_mismatch"
	// ErrApplyUnavailable is returned when the commit RPC sees a valid
	// staged descriptor but no Applier is wired to actually trigger
	// the slot-switch + reboot. fabric-update ships with a refusing
	// Applier so we never lie to the CM5 about apply success on a
	// branch where the apply path doesn't exist; fabric-security
	// supplies a real Applier and the refusal goes away.
	ErrApplyUnavailable = "apply_unavailable"
)

// SoftwareFact is the retained payload at state/self/software per
// docs/firmware-alignment-update.md §"Identity facts". `boot_id` is
// generated per boot (W6, RAM-only); `payload_sha256` is bare 64-char
// lower-hex sourced from the abupdate metadata block.
type SoftwareFact struct {
	Version       string `json:"version"`
	BuildID       string `json:"build_id"`
	ImageID       string `json:"image_id"`
	BootID        string `json:"boot_id"`
	PayloadSHA256 string `json:"payload_sha256,omitempty"`
}

// UpdaterFact is the retained payload at state/self/updater. Nullable
// fields are pointers so JSON publishes explicit nulls, not omitted
// properties, when no value is present.
type UpdaterFact struct {
	State          State   `json:"state"`
	LastError      *string `json:"last_error"`
	PendingVersion *string `json:"pending_version"`
	PendingImageID *string `json:"pending_image_id"`
	StagedImageID  *string `json:"staged_image_id"`
	JobID          *string `json:"job_id"`
}

// HealthFact is the retained payload at state/self/health. Lua extracts
// `state`; Reason is optional.
type HealthFact struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// StagedDescriptor is the metadata about a staged image, persisted in
// the abupdate metadata block by updater/main staging after the verifier
// accepts. Read at the next prepare/commit RPC to know what's actually
// stageable.
type StagedDescriptor struct {
	Version       string `json:"version"`
	BuildID       string `json:"build_id"`
	ImageID       string `json:"image_id"`
	Length        uint32 `json:"length"`
	Slot          uint8  `json:"slot"`
	PayloadSHA256 string `json:"payload_sha256"`
}

// StagePayload is the local updater/main staging RPC invoked by fabric
// after xfer_commit has verified size and transfer digest. It replaces
// the older meta.receiver/raw-member receive path; the CM5 supplies only
// target="updater/main" on the wire.
type StagePayload struct {
	LinkID    string `json:"link_id"`
	XferID    string `json:"xfer_id"`
	Target    string `json:"target"`
	Size      uint32 `json:"size"`
	DigestAlg string `json:"digest_alg"`
	Digest    string `json:"digest"`
	Meta      any    `json:"meta,omitempty"`
	Artefact  []byte `json:"artefact,omitempty"`
}

type StageReply struct {
	OK    bool   `json:"ok"`
	Err   string `json:"err,omitempty"`
	Stage string `json:"stage,omitempty"`
}

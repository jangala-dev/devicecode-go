package updater

import (
	"errors"
	"io"
)

// Manifest is the small subset of the signed-image manifest that updater
// staging needs after verification succeeds. The full canonical manifest lives
// in pico2-a-b/imagev1; this type is the local interface carried across the
// staging -> updater -> state/self/updater pipeline.
type Manifest struct {
	Version       string
	BuildID       string
	ImageID       string
	PayloadSHA256 string
	PayloadLength uint32
}

// SlotSink is what the verifier writes verified payload bytes into.
// In production this lands in the inactive abupdate slot; in tests it
// can be backed by a bytes.Buffer or similar. Keep the interface tiny.
type SlotSink interface {
	io.Writer
	// Commit finalises the staged write. Called after the verifier has
	// finished streaming and confirms the payload SHA-256 matches the
	// manifest. Returns the descriptor-relevant fields.
	Commit() error
	// Abort rolls back any partial write so the next prepare/commit
	// starts from a clean slot.
	Abort() error
}

// Verifier is updater/main staging's hook into signed-image verification.
// Production wiring uses SignedImageVerifier; tests may pass fakes, and nil
// Options.Verifier falls back to the rejecting StubVerifier.
type Verifier interface {
	// Verify reads the artefact bytes from r, validates the signed
	// envelope (header + manifest + signature), and on success streams
	// the verified payload into sink. Returns the trusted manifest the
	// staging path propagates to the staged descriptor and software fact.
	//
	// On success, Verify owns sink.Commit before it returns the manifest. On
	// failure, Verify owns sink.Abort before returning so staging does not
	// have a second sink finalization path.
	Verify(r io.Reader, sink SlotSink) (Manifest, error)
}

// ErrUnsignedNotSupported is the sentinel returned by the production
// stub on this branch. The wire `last_error` value is set to its
// Error() string so Lua-side test harnesses can grep for it.
var ErrUnsignedNotSupported = errors.New("verifier_stub: unsigned images not supported on this build")

// Applier is the slot-switch + reboot hook for the commit RPC. Split in two so
// handleCommit can publish the rebooting retain and reply accepted before the
// reboot fires; an implementation that reboots inside Apply would otherwise
// skip both the wire reply and the state/self/updater retain.
//
// New() still defaults to RefusingApplier so tests and host builds never claim
// apply success without an explicit production applier. Reactor wiring supplies
// the abupdate-backed implementation that triggers REBOOT_TYPE_FLASH_UPDATE into
// the staged slot.
type Applier interface {
	// CanApply validates that the apply path is wired and the
	// descriptor is acceptable. Quick, no side effects beyond minimal
	// validation. Errors here surface in the commit reply as
	// {ok:false, error:<msg>}; the canonical committing/rebooting
	// retains are NOT published.
	CanApply(d StagedDescriptor) error

	// ArmReboot schedules the slot-switch + reboot. Called only AFTER
	// handleCommit has published state=rebooting and replied accepted to the
	// caller. Real implementations may reboot inside this call (it
	// won't return); the spec contract is that callers must do their
	// pre-reboot work first. If it returns an error, the updater service
	// publishes that failure from its own Run loop.
	ArmReboot(d StagedDescriptor) error
}

// refusingApplier is the production default. CanApply always returns
// ErrApplyUnavailable so commit refuses with
// `error: "commit_failed"` and never reaches ArmReboot.
type refusingApplier struct{}

// RefusingApplier returns the safe-default Applier for this branch.
func RefusingApplier() Applier { return refusingApplier{} }

func (refusingApplier) CanApply(d StagedDescriptor) error {
	_ = d
	return errors.New(ErrApplyUnavailable)
}

// ArmReboot is a contract-required no-op for the refusing default —
// CanApply rejects every descriptor, so the commit handler never
// calls this. Defined for interface conformance.
func (refusingApplier) ArmReboot(d StagedDescriptor) error {
	_ = d
	return nil
}

// stubVerifier is the safe default when no verifier is wired. It always rejects
// so no unsigned firmware can stage accidentally.
type stubVerifier struct{}

// StubVerifier returns the rejecting verifier used as New's default.
func StubVerifier() Verifier { return stubVerifier{} }

func (stubVerifier) Verify(r io.Reader, sink SlotSink) (Manifest, error) {
	_ = r
	if sink != nil {
		_ = sink.Abort()
	}
	return Manifest{}, ErrUnsignedNotSupported
}

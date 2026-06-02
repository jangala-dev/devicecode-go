package updater

import (
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/otadiag"
)

// handlePrepare processes cap/self/updater/main/rpc/prepare-update after
// Fabric remaps it to the local bus. Success returns the current contract's
// prepare acknowledgement, including the required transfer target and maximum
// raw chunk size.
func (s *Service) handlePrepare(msg *bus.Message) {
	prepareAt := time.Now()
	req, ok := jsonDecode[PrepareRequest](msg.Payload)
	if !ok {
		otadiag.Event("[updater-stream]", "prepare_reject", otadiag.XferNone, otadiag.KV("reason", "bad_request"))
		s.reply(msg, Reply{OK: false, Error: "bad_request"})
		return
	}
	otadiag.Event(
		"[updater-stream]", "prepare_rx", otadiag.XferNone,
		otadiag.KV("target", req.Target),
		otadiag.KV("job_id", req.JobID),
		otadiag.KV("expected_image_id", req.ExpectedImageID),
	)
	if req.Target != "" && req.Target != PrepareTargetMCU {
		otadiag.Event("[updater-stream]", "prepare_reject", otadiag.XferNone, otadiag.KV("reason", ErrTargetMismatch))
		s.reply(msg, Reply{OK: false, Error: ErrTargetMismatch})
		return
	}

	s.mu.Lock()
	if s.preparing ||
		s.streamLeaseActive ||
		s.state == StateReceiving ||
		s.state == StateCommitting ||
		s.state == StateRebooting {
		s.mu.Unlock()
		otadiag.Event("[updater-stream]", "prepare_reject", otadiag.XferNone, otadiag.KV("reason", ErrBusy))
		s.reply(msg, Reply{OK: false, Error: ErrBusy})
		return
	}
	s.preparing = true
	s.mu.Unlock()
	otadiag.StartUpdateWindow("prepare", otadiag.XferNone)
	otadiag.Event("[updater-stream]", "prepare_start", otadiag.XferNone)
	prepareActive := true
	finishPrepare := func() {
		if prepareActive {
			s.markPrepareDone()
			prepareActive = false
		}
	}
	defer finishPrepare()
	s.setJobContext(req.JobID, req.ExpectedImageID)
	s.transitionTo(StatePreparing, "", "")

	// Clear any persisted staged descriptor from a previous successful
	// stage. Without this, a flow of (stage A) -> (prepare for B) ->
	// (stage B fails) leaves descriptor A persisted and committable —
	// which would be a real safety bug since the user-intent on
	// prepare(B) is "I want to stage B, throw away A".
	if err := s.metadataWrite.ClearStagedDescriptor(); err != nil {
		errMsg := "metadata_clear_failed:" + err.Error()
		s.transitionTo(StateFailed, errMsg, "")
		finishPrepare()
		otadiag.Event(
			"[updater-stream]", "prepare_error", otadiag.XferNone,
			otadiag.KV("err", errMsg),
			otadiag.KV("dur_ms", int(time.Since(prepareAt)/time.Millisecond)),
		)
		otadiag.StopUpdateWindow("prepare_error")
		s.reply(msg, Reply{OK: false, Error: errMsg})
		return
	}

	s.mu.Lock()
	gen := s.openStageGenerationLocked()
	snap := s.diagSnapshotLocked()
	s.mu.Unlock()
	setDiagSnapshot(snap)
	otadiag.Event("[updater-stream]", "prepare_generation", otadiag.XferNone, otadiag.KV("generation", gen))

	s.transitionTo(StateReady, "", "")
	finishPrepare()
	otadiag.Event(
		"[updater-stream]", "prepare_done", otadiag.XferNone,
		otadiag.KV("generation", gen),
		otadiag.KV("dur_ms", int(time.Since(prepareAt)/time.Millisecond)),
	)
	s.reply(msg, PrepareReply{
		Ready:        true,
		Target:       TargetUpdaterMain,
		MaxChunkSize: DefaultMaxChunkSize,
	})
}

// handleCommit processes cap/self/updater/main/rpc/commit-update after Fabric
// remaps it to the local bus. It only accepts a valid staged descriptor
// matching the requested/remembered expected image.
func (s *Service) handleCommit(msg *bus.Message) {
	req, ok := jsonDecode[CommitRequest](msg.Payload)
	if !ok {
		s.reply(msg, Reply{OK: false, Error: "bad_request"})
		return
	}

	desc, present := s.metadata.StagedDescriptor()
	s.mu.Lock()
	stagedInState := s.state == StateStaged
	pendingImageID := s.pendingImageID
	streamActive := s.streamLeaseActive
	s.mu.Unlock()

	if streamActive {
		s.reply(msg, Reply{OK: false, Error: ErrBusy})
		return
	}
	if !present || !stagedInState {
		s.reply(msg, Reply{OK: false, Error: ErrNothingStaged})
		return
	}
	expectedImageID := req.ExpectedImageID
	if expectedImageID == "" {
		expectedImageID = pendingImageID
	}
	if expectedImageID != "" && desc.ImageID != expectedImageID {
		s.reply(msg, Reply{OK: false, Error: ErrTargetMismatch})
		return
	}

	// Validate the apply path before publishing committing/rebooting or
	// replying accepted. The default Applier refuses in non-hardware tests.
	if err := s.applier.CanApply(desc); err != nil {
		s.reply(msg, Reply{OK: false, Error: err.Error()})
		return
	}

	s.transitionTo(StateCommitting, "", desc.Version)
	s.reply(msg, CommitReply{Accepted: true, RebootRequired: true})
	s.transitionTo(StateRebooting, "", desc.Version)

	scheduleArmReboot(s.applier, desc, s.applyResults)
}

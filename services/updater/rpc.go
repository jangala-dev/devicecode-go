package updater

// prepare processes cap/self/updater/main/rpc/prepare-update after Fabric remaps
// it to the local endpoint. Success returns the current contract's prepare
// acknowledgement, including the required transfer target and maximum raw chunk
// size.
func (s *Service) prepare(payload any) any {
	req, ok := jsonDecode[PrepareRequest](payload)
	if !ok {
		logUpdaterPrepareResult(false, ErrInvalidRequest, 0)
		logUpdaterCommitReject(ErrInvalidRequest)
		return Reply{OK: false, Error: ErrInvalidRequest}
	}
	if req.Target != "" && req.Target != PrepareTargetMCU {
		logUpdaterPrepareResult(false, ErrUnsupportedTarget, 0)
		return Reply{OK: false, Error: ErrUnsupportedTarget}
	}

	s.mu.Lock()
	if s.preparing ||
		s.streamLeaseActive ||
		s.state == StateReceiving ||
		s.state == StateCommitting ||
		s.state == StateRebooting {
		s.mu.Unlock()
		logUpdaterPrepareResult(false, ErrBusy, 0)
		logUpdaterCommitReject(ErrBusy)
		return Reply{OK: false, Error: ErrBusy}
	}
	s.preparing = true
	s.mu.Unlock()
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

	// Clear any persisted staged descriptor from a previous successful stage.
	if err := s.metadataWrite.ClearStagedDescriptor(); err != nil {
		errMsg := "metadata_clear_failed:" + err.Error()
		s.transitionTo(StateFailed, errMsg, "")
		finishPrepare()
		logUpdaterPrepareResult(false, errMsg, 0)
		return Reply{OK: false, Error: errMsg}
	}

	s.mu.Lock()
	gen := s.openStageGenerationLocked()
	s.mu.Unlock()

	s.transitionTo(StateReady, "", "")
	finishPrepare()
	logUpdaterPrepareResult(true, "", gen)
	return PrepareReply{Ready: true, Target: TargetUpdaterMain, MaxChunkSize: DefaultMaxChunkSize}
}

func (s *Service) rememberAcceptedCommit(req CommitRequest, expectedImageID string, desc StagedDescriptor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCommitJobID = req.JobID
	s.lastCommitExpectedImageID = expectedImageID
	s.lastCommitImageID = desc.ImageID
	s.lastCommitToken = req.CommitToken
}

func (s *Service) duplicateAcceptedCommitLocked(req CommitRequest) bool {
	if s.state != StateCommitting && s.state != StateRebooting {
		return false
	}
	if s.lastCommitJobID == "" || req.JobID != s.lastCommitJobID {
		return false
	}
	if s.lastCommitToken == "" || req.CommitToken != s.lastCommitToken {
		return false
	}
	if req.ExpectedImageID != "" {
		return req.ExpectedImageID == s.lastCommitExpectedImageID || req.ExpectedImageID == s.lastCommitImageID
	}
	return true
}

// commit processes cap/self/updater/main/rpc/commit-update after Fabric remaps
// it to the local endpoint. It only accepts a valid staged descriptor matching
// the requested/remembered expected image.
func (s *Service) commit(payload any) any {
	req, ok := jsonDecode[CommitRequest](payload)
	if !ok {
		return Reply{OK: false, Error: ErrInvalidRequest}
	}

	s.mu.Lock()
	if s.duplicateAcceptedCommitLocked(req) {
		s.mu.Unlock()
		return CommitReply{Accepted: true, RebootRequired: true}
	}
	s.mu.Unlock()

	desc, present := s.metadata.StagedDescriptor()
	s.mu.Lock()
	stagedInState := s.state == StateStaged
	pendingImageID := s.pendingImageID
	streamActive := s.streamLeaseActive
	s.mu.Unlock()

	if streamActive {
		logUpdaterCommitReject(ErrBusy)
		return Reply{OK: false, Error: ErrBusy}
	}
	if !present || !stagedInState {
		logUpdaterCommitReject(ErrNoStagedImage)
		return Reply{OK: false, Error: ErrNoStagedImage}
	}
	expectedImageID := pendingImageID
	if expectedImageID == "" {
		expectedImageID = req.ExpectedImageID
	}
	if expectedImageID != "" && desc.ImageID != expectedImageID {
		logUpdaterCommitReject(ErrImageIDMismatch)
		return Reply{OK: false, Error: ErrImageIDMismatch}
	}
	if err := s.applier.CanApply(desc); err != nil {
		logUpdaterCommitReject(ErrApplyUnavailable)
		return Reply{OK: false, Error: ErrApplyUnavailable}
	}

	s.rememberAcceptedCommit(req, expectedImageID, desc)
	logUpdaterCommitAccepted(req.JobID, desc.ImageID, desc.Version, desc.Length, int(desc.Slot))
	s.transitionTo(StateCommitting, "", desc.Version)
	reply := CommitReply{Accepted: true, RebootRequired: true}
	s.transitionTo(StateRebooting, "", desc.Version)
	scheduleArmReboot(s.applier, desc, s.applyResults)
	return reply
}

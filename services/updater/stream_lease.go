package updater

import (
	"errors"
	"sync"
)

var (
	activeServiceMu sync.Mutex
	activeService   *Service
)

func registerActiveService(s *Service) func() {
	activeServiceMu.Lock()
	activeService = s
	activeServiceMu.Unlock()
	return func() {
		activeServiceMu.Lock()
		if activeService == s {
			activeService = nil
		}
		activeServiceMu.Unlock()
	}
}

func currentService() *Service {
	activeServiceMu.Lock()
	defer activeServiceMu.Unlock()
	return activeService
}

// BeginStreamedStage acquires the updater-owned staging lease opened by the
// last successful prepare-update call. Fabric calls this from xfer_begin before
// any sink mutates flash or buffers transfer state.
func BeginStreamedStage(xferID string, size uint32) (uint64, error) {
	s := currentService()
	if s == nil {
		return 0, errors.New("updater_not_running")
	}
	gen, err := s.beginStreamedStageLease(xferID)
	if err != nil {
		return 0, err
	}
	if err := startStreamedStage(size); err != nil {
		s.cancelStreamedStageLease(xferID, gen, err.Error())
		return 0, err
	}
	if err := s.markStreamedStageReceiving(xferID, gen); err != nil {
		abortStreamedStage()
		s.cancelStreamedStageLease(xferID, gen, err.Error())
		return 0, err
	}
	return gen, nil
}

func WriteStreamedStage(xferID string, generation uint64, data []byte) error {
	s := currentService()
	if s == nil {
		return errors.New("updater_not_running")
	}
	if err := s.checkStreamedStageLease(xferID, generation, false); err != nil {
		return err
	}
	return writeStreamedStage(data)
}

func CommitStreamedStage(xferID string, generation uint64) (uint32, error) {
	s := currentService()
	if s == nil {
		return 0, errors.New("updater_not_running")
	}
	if err := s.checkStreamedStageLease(xferID, generation, false); err != nil {
		return 0, err
	}
	staged, err := commitStreamedStage()
	if err != nil {
		s.cancelStreamedStageLease(xferID, generation, err.Error())
		return 0, err
	}
	if err := s.markStreamedStageCommitted(xferID, generation); err != nil {
		abortStreamedStage()
		return 0, err
	}
	return staged.Length, nil
}

func CommitBufferedStage(xferID string, generation uint64) error {
	s := currentService()
	if s == nil {
		return errors.New("updater_not_running")
	}
	return s.markStreamedStageCommitted(xferID, generation)
}

func AbortStreamedStage(xferID string, generation uint64, reason string) {
	abortStreamedStage()
	if s := currentService(); s != nil {
		s.cancelStreamedStageLease(xferID, generation, reason)
	}
}

func CancelStreamedStage(xferID string, generation uint64, reason string) {
	AbortStreamedStage(xferID, generation, reason)
}

func (s *Service) openStageGenerationLocked() uint64 {
	s.stageGeneration++
	if s.stageGeneration == 0 {
		s.stageGeneration = 1
	}
	s.streamLeaseActive = false
	s.streamXferID = ""
	s.streamCancelled = false
	s.streamCommitted = false
	discardStreamedStageResult()
	return s.stageGeneration
}

func (s *Service) beginStreamedStageLease(xferID string) (uint64, error) {
	if xferID == "" {
		return 0, errors.New("bad_message:xfer_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preparing ||
		s.state == StatePreparing ||
		s.state == StateCommitting ||
		s.state == StateRebooting ||
		s.state == StateReceiving ||
		s.streamLeaseActive {
		return 0, errors.New(ErrBusy)
	}
	if s.state != StateReady || s.stageGeneration == 0 {
		return 0, errors.New("stage_not_prepared")
	}
	s.streamLeaseActive = true
	s.streamXferID = xferID
	s.streamCancelled = false
	s.streamCommitted = false
	return s.stageGeneration, nil
}

func (s *Service) markStreamedStageReceiving(xferID string, generation uint64) error {
	s.mu.Lock()
	if generation == 0 || generation != s.stageGeneration || xferID == "" || xferID != s.streamXferID {
		s.mu.Unlock()
		return errors.New("stage_generation_mismatch")
	}
	if s.streamCancelled {
		s.mu.Unlock()
		return errors.New("stage_cancelled")
	}
	if !s.streamLeaseActive {
		s.mu.Unlock()
		return errors.New("stage_not_active")
	}
	s.state = StateReceiving
	s.lastError = ""
	s.mu.Unlock()
	s.PublishUpdater()
	return nil
}

func (s *Service) checkStreamedStageLease(xferID string, generation uint64, requireCommitted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == 0 || generation != s.stageGeneration || xferID == "" || xferID != s.streamXferID {
		return errors.New("stage_generation_mismatch")
	}
	if s.streamCancelled {
		return errors.New("stage_cancelled")
	}
	if !s.streamLeaseActive {
		return errors.New("stage_not_active")
	}
	if requireCommitted && !s.streamCommitted {
		return errors.New("stage_not_committed")
	}
	return nil
}

func (s *Service) markStreamedStageCommitted(xferID string, generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == 0 || generation != s.stageGeneration || xferID == "" || xferID != s.streamXferID {
		return errors.New("stage_generation_mismatch")
	}
	if s.streamCancelled {
		return errors.New("stage_cancelled")
	}
	if !s.streamLeaseActive {
		return errors.New("stage_not_active")
	}
	s.streamCommitted = true
	return nil
}

func (s *Service) cancelStreamedStageLease(xferID string, generation uint64, reason string) bool {
	s.mu.Lock()
	matches := generation != 0 &&
		generation == s.stageGeneration &&
		s.streamLeaseActive &&
		(xferID == "" || xferID == s.streamXferID)
	if matches {
		s.streamCancelled = true
		s.streamLeaseActive = false
		s.streamCommitted = false
		s.streamXferID = ""
		s.stagedImageID = ""
		s.pendingVersion = ""
		if s.state == StateReady || s.state == StateReceiving || s.state == StateStaged {
			s.state = StateFailed
		}
		if reason != "" {
			s.lastError = reason
		}
	}
	s.mu.Unlock()
	if matches {
		_ = s.metadataWrite.ClearStagedDescriptor()
		s.PublishUpdater()
	}
	return matches
}

func (s *Service) releaseStreamedStageLease(xferID string, generation uint64) bool {
	s.mu.Lock()
	matches := generation != 0 &&
		generation == s.stageGeneration &&
		xferID == s.streamXferID &&
		s.streamLeaseActive &&
		!s.streamCancelled
	if matches {
		s.streamLeaseActive = false
		s.streamCommitted = false
		s.streamXferID = ""
	}
	s.mu.Unlock()
	return matches
}

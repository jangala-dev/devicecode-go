package updater

import (
	"errors"
	"sync"
	"time"

	"devicecode-go/services/otadiag"
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
	beginAt := time.Now()
	otadiag.SetActiveXfer(xferID)
	otadiag.Event("[updater-stream]", "begin_entry", xferID, otadiag.KV("size", size))
	s := currentService()
	if s == nil {
		otadiag.Event(
			"[updater-stream]", "begin_error", xferID,
			otadiag.KV("err", "updater_not_running"),
			otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
		)
		otadiag.StopUpdateWindow("updater_not_running")
		return 0, errors.New("updater_not_running")
	}
	gen, err := s.beginStreamedStageLease(xferID)
	if err != nil {
		otadiag.Event(
			"[updater-stream]", "lease_error", xferID,
			otadiag.KV("err", err.Error()),
			otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
		)
		return 0, err
	}
	otadiag.Event("[updater-stream]", "lease_ok", xferID, otadiag.KV("generation", gen))
	installABUpdateDiagHook(xferID, gen)
	startAt := time.Now()
	otadiag.Event(
		"[updater-stream]", "start_entry", xferID,
		otadiag.KV("generation", gen),
		otadiag.KV("size", size),
	)
	if err := startStreamedStage(xferID, gen, size); err != nil {
		otadiag.Event(
			"[updater-stream]", "start_error", xferID,
			otadiag.KV("generation", gen),
			otadiag.KV("err", err.Error()),
			otadiag.KV("dur_ms", int(time.Since(startAt)/time.Millisecond)),
		)
		clearABUpdateDiagHook()
		s.cancelStreamedStageLease(xferID, gen, err.Error())
		otadiag.Event(
			"[updater-stream]", "begin_error", xferID,
			otadiag.KV("err", err.Error()),
			otadiag.KV("generation", gen),
			otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
		)
		otadiag.StopUpdateWindow("start_streamed_stage_error")
		return 0, err
	}
	otadiag.Event(
		"[updater-stream]", "start_exit", xferID,
		otadiag.KV("generation", gen),
		otadiag.KV("dur_ms", int(time.Since(startAt)/time.Millisecond)),
	)
	markAt := time.Now()
	otadiag.Event("[updater-stream]", "mark_receiving_entry", xferID, otadiag.KV("generation", gen))
	if err := s.markStreamedStageReceiving(xferID, gen); err != nil {
		otadiag.Event(
			"[updater-stream]", "mark_receiving_error", xferID,
			otadiag.KV("generation", gen),
			otadiag.KV("err", err.Error()),
			otadiag.KV("dur_ms", int(time.Since(markAt)/time.Millisecond)),
		)
		abortStreamedStage()
		clearABUpdateDiagHook()
		s.cancelStreamedStageLease(xferID, gen, err.Error())
		otadiag.Event(
			"[updater-stream]", "begin_error", xferID,
			otadiag.KV("err", err.Error()),
			otadiag.KV("generation", gen),
			otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
		)
		otadiag.StopUpdateWindow("mark_receiving_error")
		return 0, err
	}
	otadiag.Event(
		"[updater-stream]", "mark_receiving_exit", xferID,
		otadiag.KV("generation", gen),
		otadiag.KV("dur_ms", int(time.Since(markAt)/time.Millisecond)),
	)
	otadiag.Event(
		"[updater-stream]", "begin_exit", xferID,
		otadiag.KV("generation", gen),
		otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
	)
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
	return writeStreamedStage(xferID, generation, data)
}

func CommitStreamedStage(xferID string, generation uint64) (uint32, error) {
	s := currentService()
	if s == nil {
		return 0, errors.New("updater_not_running")
	}
	if err := s.checkStreamedStageLease(xferID, generation, false); err != nil {
		return 0, err
	}
	staged, err := commitStreamedStage(xferID, generation)
	clearABUpdateDiagHook()
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
	if err := s.markStreamedStageCommitted(xferID, generation); err != nil {
		return err
	}
	clearABUpdateDiagHook()
	return nil
}

func AbortStreamedStage(xferID string, generation uint64, reason string) {
	abortStreamedStage()
	clearABUpdateDiagHook()
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
	if s.preparing ||
		s.state == StatePreparing ||
		s.state == StateCommitting ||
		s.state == StateRebooting ||
		s.state == StateReceiving ||
		s.streamLeaseActive {
		s.mu.Unlock()
		return 0, errors.New(ErrBusy)
	}
	if s.state != StateReady || s.stageGeneration == 0 {
		s.mu.Unlock()
		return 0, errors.New("stage_not_prepared")
	}
	s.streamLeaseActive = true
	s.streamXferID = xferID
	s.streamCancelled = false
	s.streamCommitted = false
	snap := s.diagSnapshotLocked()
	gen := s.stageGeneration
	s.mu.Unlock()
	setDiagSnapshot(snap)
	return gen, nil
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
	s.PublishCriticalFacts()
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
	snap := s.diagSnapshotLocked()
	s.mu.Unlock()
	setDiagSnapshot(snap)
	if matches {
		_ = s.metadataWrite.ClearStagedDescriptor()
		s.PublishCriticalFacts()
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

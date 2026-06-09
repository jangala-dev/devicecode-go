package updater

import (
	"context"
	"errors"
	"time"

	"devicecode-go/services/otadiag"
)

type streamedStageCommandKind uint8

const (
	streamedStageCommandBegin streamedStageCommandKind = iota + 1
	streamedStageCommandWrite
	streamedStageCommandCommit
	streamedStageCommandAbort
	streamedStageCommandCancel
)

type streamedStageCommand struct {
	kind       streamedStageCommandKind
	xferID     string
	generation uint64
	size       uint32
	data       []byte
	reason     string
	reply      chan streamedStageCommandResult
}

type streamedStageCommandResult struct {
	generation uint64
	written    uint32
	err        error
}

type streamedStageWorkerCommand struct {
	kind       streamedStageCommandKind
	xferID     string
	generation uint64
	size       uint32
	data       []byte
	reason     string
}

type streamedStageWorkerResult struct {
	kind       streamedStageCommandKind
	xferID     string
	generation uint64
	staged     streamedStage
	err        error
}

// BeginStreamedStage submits a transfer-begin operation to the updater reactor.
// The updater loop owns the lease/state decision; the stage worker owns any
// verifier or flash setup needed to accept the stream.
func (s *Service) BeginStreamedStage(xferID string, size uint32) (uint64, error) {
	res := s.submitStreamedStageCommand(streamedStageCommand{
		kind:   streamedStageCommandBegin,
		xferID: xferID,
		size:   size,
	})
	return res.generation, res.err
}

func (s *Service) WriteStreamedStage(xferID string, generation uint64, data []byte) error {
	res := s.submitStreamedStageCommand(streamedStageCommand{
		kind:       streamedStageCommandWrite,
		xferID:     xferID,
		generation: generation,
		data:       data,
	})
	return res.err
}

func (s *Service) CommitStreamedStage(xferID string, generation uint64) (uint32, error) {
	res := s.submitStreamedStageCommand(streamedStageCommand{
		kind:       streamedStageCommandCommit,
		xferID:     xferID,
		generation: generation,
	})
	return res.written, res.err
}

func (s *Service) AbortStreamedStage(xferID string, generation uint64, reason string) {
	_ = s.submitStreamedStageCommand(streamedStageCommand{
		kind:       streamedStageCommandAbort,
		xferID:     xferID,
		generation: generation,
		reason:     reason,
	})
}

func (s *Service) CancelStreamedStage(xferID string, generation uint64, reason string) {
	_ = s.submitStreamedStageCommand(streamedStageCommand{
		kind:       streamedStageCommandCancel,
		xferID:     xferID,
		generation: generation,
		reason:     reason,
	})
}

func (s *Service) submitStreamedStageCommand(cmd streamedStageCommand) streamedStageCommandResult {
	if s == nil {
		return streamedStageCommandResult{err: errors.New("updater_not_running")}
	}
	select {
	case <-s.stageStopped:
		return streamedStageCommandResult{err: errors.New("updater_not_running")}
	default:
	}
	select {
	case <-s.stageReady:
	default:
		return streamedStageCommandResult{err: errors.New("updater_not_running")}
	}
	cmd.reply = make(chan streamedStageCommandResult, 1)
	select {
	case s.stageCommands <- cmd:
	case <-s.stageStopped:
		return streamedStageCommandResult{err: errors.New("updater_not_running")}
	}
	select {
	case res := <-cmd.reply:
		return res
	case <-s.stageStopped:
		return streamedStageCommandResult{err: errors.New("updater_not_running")}
	}
}

func (s *Service) runStreamedStageWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			abortStreamedStage()
			s.clearActiveABUpdateDiagHook()
			return
		case cmd, ok := <-s.stageWorkerCommands:
			if !ok {
				abortStreamedStage()
				s.clearActiveABUpdateDiagHook()
				return
			}
			res := streamedStageWorkerResult{kind: cmd.kind, xferID: cmd.xferID, generation: cmd.generation}
			switch cmd.kind {
			case streamedStageCommandBegin:
				res.err = startStreamedStage(cmd.xferID, cmd.generation, cmd.size)
			case streamedStageCommandWrite:
				res.err = writeStreamedStage(cmd.xferID, cmd.generation, cmd.data)
			case streamedStageCommandCommit:
				res.staged, res.err = commitStreamedStage(s, cmd.xferID, cmd.generation)
			case streamedStageCommandAbort, streamedStageCommandCancel:
				abortStreamedStage()
				clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
			default:
				res.err = errors.New("bad_stage_command")
			}
			select {
			case s.stageWorkerResults <- res:
			case <-ctx.Done():
				abortStreamedStage()
				s.clearActiveABUpdateDiagHook()
				return
			}
		}
	}
}

func (s *Service) handleStreamedStageCommand(cmd streamedStageCommand) {
	if cmd.reply == nil {
		return
	}
	if s.pendingStageCommand != nil {
		switch cmd.kind {
		case streamedStageCommandAbort, streamedStageCommandCancel:
			s.cancelPendingStreamedStage(cmd)
		default:
			cmd.reply <- streamedStageCommandResult{err: errors.New(ErrBusy)}
		}
		return
	}
	switch cmd.kind {
	case streamedStageCommandBegin:
		s.startStreamedStageBegin(cmd)
	case streamedStageCommandWrite:
		s.startStreamedStageWrite(cmd)
	case streamedStageCommandCommit:
		s.startStreamedStageCommit(cmd)
	case streamedStageCommandAbort, streamedStageCommandCancel:
		s.startStreamedStageAbort(cmd)
	default:
		cmd.reply <- streamedStageCommandResult{err: errors.New("bad_stage_command")}
	}
}

func (s *Service) cancelPendingStreamedStage(cmd streamedStageCommand) {
	if cmd.reason == "" {
		cmd.reason = "abort"
	}
	// The updater reactor owns logical cancellation even while a flash/verifier
	// worker command is in progress. The worker cannot be interrupted inside a
	// bounded operation, but its eventual result will be rejected by the lease
	// checks because the lease is cancelled here first.
	s.cancelStreamedStageLease(cmd.xferID, cmd.generation, cmd.reason)
	clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
	s.queueStageWorkerAbort(cmd.xferID, cmd.generation, cmd.reason)
	cmd.reply <- streamedStageCommandResult{}
}

func (s *Service) startStreamedStageBegin(cmd streamedStageCommand) {
	beginAt := time.Now()
	otadiag.SetActiveXfer(cmd.xferID)
	otadiag.Event("[updater-stream]", "begin_entry", cmd.xferID, otadiag.KV("size", cmd.size))
	gen, err := s.beginStreamedStageLease(cmd.xferID)
	if err != nil {
		otadiag.Event(
			"[updater-stream]", "lease_error", cmd.xferID,
			otadiag.KV("err", err.Error()),
			otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
		)
		cmd.reply <- streamedStageCommandResult{err: err}
		return
	}
	otadiag.Event("[updater-stream]", "lease_ok", cmd.xferID, otadiag.KV("generation", gen))
	installABUpdateDiagHook(cmd.xferID, gen)
	cmd.generation = gen
	s.pendingStageCommand = &cmd
	s.sendStageWorkerCommand(streamedStageWorkerCommand{
		kind:       streamedStageCommandBegin,
		xferID:     cmd.xferID,
		generation: gen,
		size:       cmd.size,
	})
}

func (s *Service) startStreamedStageWrite(cmd streamedStageCommand) {
	if err := s.checkStreamedStageLease(cmd.xferID, cmd.generation, false); err != nil {
		cmd.reply <- streamedStageCommandResult{err: err}
		return
	}
	s.pendingStageCommand = &cmd
	s.sendStageWorkerCommand(streamedStageWorkerCommand{
		kind:       streamedStageCommandWrite,
		xferID:     cmd.xferID,
		generation: cmd.generation,
		data:       cmd.data,
	})
}

func (s *Service) startStreamedStageCommit(cmd streamedStageCommand) {
	if err := s.checkStreamedStageLease(cmd.xferID, cmd.generation, false); err != nil {
		cmd.reply <- streamedStageCommandResult{err: err}
		return
	}
	s.pendingStageCommand = &cmd
	s.sendStageWorkerCommand(streamedStageWorkerCommand{
		kind:       streamedStageCommandCommit,
		xferID:     cmd.xferID,
		generation: cmd.generation,
	})
}

func (s *Service) startStreamedStageAbort(cmd streamedStageCommand) {
	if cmd.reason == "" {
		cmd.reason = "abort"
	}
	// Mark the logical lease cancelled in the updater reactor first. The
	// worker then performs the storage/verifier abort at its next safe point.
	if cmd.kind == streamedStageCommandCancel {
		s.cancelStreamedStageLease(cmd.xferID, cmd.generation, cmd.reason)
	} else {
		s.cancelStreamedStageLease(cmd.xferID, cmd.generation, cmd.reason)
	}
	clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
	s.pendingStageCommand = &cmd
	s.sendStageWorkerCommand(streamedStageWorkerCommand{
		kind:       cmd.kind,
		xferID:     cmd.xferID,
		generation: cmd.generation,
		reason:     cmd.reason,
	})
}

func (s *Service) sendStageWorkerCommand(cmd streamedStageWorkerCommand) {
	select {
	case s.stageWorkerCommands <- cmd:
	default:
		// The updater reactor admits only one pending command at a time, so the
		// worker queue should not fill. If it does, fail and cancel the logical
		// lease from the reactor rather than blocking it.
		if s.pendingStageCommand != nil && s.pendingStageCommand.reply != nil {
			pending := s.pendingStageCommand
			s.pendingStageCommand = nil
			if pending.generation != 0 {
				s.cancelStreamedStageLease(pending.xferID, pending.generation, ErrBusy)
				clearABUpdateDiagHookFor(pending.xferID, pending.generation)
			}
			pending.reply <- streamedStageCommandResult{err: errors.New(ErrBusy)}
		}
	}
}

func (s *Service) handleStreamedStageWorkerResult(res streamedStageWorkerResult) {
	cmd := s.pendingStageCommand
	if cmd == nil || cmd.xferID != res.xferID || cmd.generation != res.generation || cmd.kind != res.kind {
		// Stale worker result from an already-cancelled generation. The updater
		// reactor is authoritative, so ignore it.
		return
	}
	s.pendingStageCommand = nil
	switch res.kind {
	case streamedStageCommandBegin:
		s.finishStreamedStageBegin(*cmd, res)
	case streamedStageCommandWrite:
		s.finishStreamedStageWrite(*cmd, res)
	case streamedStageCommandCommit:
		s.finishStreamedStageCommit(*cmd, res)
	case streamedStageCommandAbort, streamedStageCommandCancel:
		clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
		cmd.reply <- streamedStageCommandResult{}
	default:
		cmd.reply <- streamedStageCommandResult{err: errors.New("bad_stage_command")}
	}
}

func (s *Service) finishStreamedStageBegin(cmd streamedStageCommand, res streamedStageWorkerResult) {
	beginAt := time.Now()
	if res.err != nil {
		clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
		s.cancelStreamedStageLease(cmd.xferID, cmd.generation, res.err.Error())
		otadiag.Event(
			"[updater-stream]", "begin_error", cmd.xferID,
			otadiag.KV("err", res.err.Error()),
			otadiag.KV("generation", cmd.generation),
			otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
		)
		otadiag.StopUpdateWindow("start_streamed_stage_error")
		cmd.reply <- streamedStageCommandResult{err: res.err}
		return
	}
	if err := s.markStreamedStageReceiving(cmd.xferID, cmd.generation); err != nil {
		clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
		s.cancelStreamedStageLease(cmd.xferID, cmd.generation, err.Error())
		s.queueStageWorkerAbort(cmd.xferID, cmd.generation, err.Error())
		otadiag.Event(
			"[updater-stream]", "begin_error", cmd.xferID,
			otadiag.KV("err", err.Error()),
			otadiag.KV("generation", cmd.generation),
			otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
		)
		otadiag.StopUpdateWindow("mark_receiving_error")
		cmd.reply <- streamedStageCommandResult{err: err}
		return
	}
	otadiag.Event(
		"[updater-stream]", "begin_exit", cmd.xferID,
		otadiag.KV("generation", cmd.generation),
		otadiag.KV("dur_ms", int(time.Since(beginAt)/time.Millisecond)),
	)
	cmd.reply <- streamedStageCommandResult{generation: cmd.generation}
}

func (s *Service) finishStreamedStageWrite(cmd streamedStageCommand, res streamedStageWorkerResult) {
	if res.err != nil {
		s.cancelStreamedStageLease(cmd.xferID, cmd.generation, res.err.Error())
		clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
		s.queueStageWorkerAbort(cmd.xferID, cmd.generation, res.err.Error())
		cmd.reply <- streamedStageCommandResult{err: res.err}
		return
	}
	if err := s.checkStreamedStageLease(cmd.xferID, cmd.generation, false); err != nil {
		clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
		s.queueStageWorkerAbort(cmd.xferID, cmd.generation, err.Error())
		cmd.reply <- streamedStageCommandResult{err: err}
		return
	}
	cmd.reply <- streamedStageCommandResult{}
}

func (s *Service) finishStreamedStageCommit(cmd streamedStageCommand, res streamedStageWorkerResult) {
	clearABUpdateDiagHookFor(cmd.xferID, cmd.generation)
	if res.err != nil {
		s.cancelStreamedStageLease(cmd.xferID, cmd.generation, res.err.Error())
		cmd.reply <- streamedStageCommandResult{err: res.err}
		return
	}
	if err := s.markStreamedStageCommitted(cmd.xferID, cmd.generation); err != nil {
		s.queueStageWorkerAbort(cmd.xferID, cmd.generation, err.Error())
		cmd.reply <- streamedStageCommandResult{err: err}
		return
	}
	s.setStreamedStageResult(res.staged)
	cmd.reply <- streamedStageCommandResult{written: res.staged.Length}
}

func (s *Service) queueStageWorkerAbort(xferID string, generation uint64, reason string) {
	if reason == "" {
		reason = "abort"
	}
	select {
	case s.stageWorkerCommands <- streamedStageWorkerCommand{kind: streamedStageCommandAbort, xferID: xferID, generation: generation, reason: reason}:
	default:
	}
}

func (s *Service) setStreamedStageResult(staged streamedStage) {
	s.mu.Lock()
	s.streamStageResult = staged
	s.streamStageResultOK = true
	s.mu.Unlock()
}

func (s *Service) clearActiveABUpdateDiagHook() {
	if s == nil {
		return
	}
	s.mu.Lock()
	xferID := s.streamXferID
	generation := s.stageGeneration
	s.mu.Unlock()
	if xferID == "" || generation == 0 {
		return
	}
	clearABUpdateDiagHookFor(xferID, generation)
}

func (s *Service) consumeStreamedStageResult() (streamedStage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.streamStageResultOK {
		return streamedStage{}, false
	}
	out := s.streamStageResult
	s.streamStageResult = streamedStage{}
	s.streamStageResultOK = false
	return out, true
}

func (s *Service) discardStreamedStageResultLocked() {
	s.streamStageResult = streamedStage{}
	s.streamStageResultOK = false
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
	s.discardStreamedStageResultLocked()
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
	s.discardStreamedStageResultLocked()
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
		s.discardStreamedStageResultLocked()
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

package fabric

import "errors"

// streamedStageSink is the updater/main transfer sink. It keeps Fabric on the
// transfer-protocol side of the boundary: all update ownership goes through the
// explicit StageController supplied by the caller.
type streamedStageSink struct {
	controller StageController
	xferID     string
	generation uint64
	accepted   uint32
	closed     bool
}

func beginUpdaterTransfer(controller StageController, meta transferMeta) (transferSink, error) {
	if controller == nil {
		return nil, errors.New("updater_stage_controller_missing")
	}
	generation, err := controller.BeginStreamedStage(meta.ID, meta.Size)
	if err != nil {
		return nil, err
	}
	return &streamedStageSink{controller: controller, xferID: meta.ID, generation: generation}, nil
}

func (s *streamedStageSink) WriteChunk(off uint32, data []byte) error {
	if s.closed {
		return errors.New("sink_closed")
	}
	if s.accepted != off {
		return errors.New("unexpected_offset")
	}
	if err := s.controller.WriteStreamedStage(s.xferID, s.generation, data); err != nil {
		return err
	}
	s.accepted += uint32(len(data))
	return nil
}

func (s *streamedStageSink) Commit() (transferInfo, error) {
	if s.closed {
		return transferInfo{}, errors.New("sink_closed")
	}
	written, err := s.controller.CommitStreamedStage(s.xferID, s.generation)
	if err != nil {
		return transferInfo{}, err
	}
	s.closed = true
	return transferInfo{
		BytesWritten: written,
		Generation:   s.generation,
		cancel:       s.cancelAfterCommit,
	}, nil
}

func (s *streamedStageSink) Apply() error { return nil }

func (s *streamedStageSink) Abort(reason string) error {
	s.controller.AbortStreamedStage(s.xferID, s.generation, reason)
	s.closed = true
	return nil
}

func (s *streamedStageSink) cancelAfterCommit(reason string) {
	s.controller.CancelStreamedStage(s.xferID, s.generation, reason)
}

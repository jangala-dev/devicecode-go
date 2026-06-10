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
	if fabricTraceEnabled {
		println("[fabric-sink]", "begin", "xfer", meta.ID, "target", meta.Target, "size", meta.Size)
	}
	if controller == nil {
		return nil, errors.New("updater_stage_controller_missing")
	}
	generation, err := controller.BeginStreamedStage(meta.ID, meta.Size)
	if err != nil {
		if fabricTraceEnabled {
			println("[fabric-sink]", "begin_error", "xfer", meta.ID, "err", err.Error())
		}
		return nil, err
	}
	if fabricTraceEnabled {
		println("[fabric-sink]", "begin_ok", "xfer", meta.ID, "generation", generation)
	}
	return &streamedStageSink{controller: controller, xferID: meta.ID, generation: generation}, nil
}

func (s *streamedStageSink) WriteChunk(off uint32, data []byte) error {
	if fabricTraceEnabled {
		println("[fabric-sink]", "write", "xfer", s.xferID, "generation", s.generation, "offset", off, "len", len(data))
	}
	if s.closed {
		return errors.New("sink_closed")
	}
	if s.accepted != off {
		return errors.New("unexpected_offset")
	}
	if err := s.controller.WriteStreamedStage(s.xferID, s.generation, data); err != nil {
		if fabricTraceEnabled {
			println("[fabric-sink]", "write_error", "xfer", s.xferID, "err", err.Error())
		}
		return err
	}
	s.accepted += uint32(len(data))
	if fabricTraceEnabled {
		println("[fabric-sink]", "write_ok", "xfer", s.xferID, "accepted", s.accepted)
	}
	return nil
}

func (s *streamedStageSink) Commit() (transferInfo, error) {
	if fabricTraceEnabled {
		println("[fabric-sink]", "commit", "xfer", s.xferID, "generation", s.generation)
	}
	if s.closed {
		return transferInfo{}, errors.New("sink_closed")
	}
	written, err := s.controller.CommitStreamedStage(s.xferID, s.generation)
	if err != nil {
		if fabricTraceEnabled {
			println("[fabric-sink]", "commit_error", "xfer", s.xferID, "err", err.Error())
		}
		return transferInfo{}, err
	}
	if fabricTraceEnabled {
		println("[fabric-sink]", "commit_ok", "xfer", s.xferID, "written", written)
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

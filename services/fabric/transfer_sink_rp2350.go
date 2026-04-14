//go:build tinygo && rp2350

package fabric

import (
	"errors"
	"fmt"
	"time"

	"ab-bringup/abupdate"
)

const rp2350TransferStageSize = 4096

var errTransferUnsupported = errors.New("unsupported")

type rp2350TransferFactory struct{}

type rp2350TransferSink struct {
	updater *abupdate.Updater

	// Stage verified transfer bytes in protocol code so flash writes happen in
	// larger batches instead of directly on every UART chunk.
	stage     [rp2350TransferStageSize]byte
	stageUsed uint32
	accepted  uint32
}

func newTransferFactory() transferFactory {
	return rp2350TransferFactory{}
}

func (rp2350TransferFactory) Begin(meta transferMeta) (transferSink, error) {
	if meta.Kind != "firmware.rp2350" || meta.Format != "bin" {
		return nil, errTransferUnsupported
	}

	var updater abupdate.Updater
	if rc := updater.Init(); rc != 0 {
		return nil, fmt.Errorf("updater_init:%d", rc)
	}
	if rc := updater.BeginUpdate(meta.Size); rc != 0 {
		return nil, fmt.Errorf("begin_update:%d", rc)
	}

	return &rp2350TransferSink{updater: &updater}, nil
}

func (s *rp2350TransferSink) flushStage(seq uint32, force bool) error {
	if s.stageUsed == 0 {
		return nil
	}

	before := s.updater.BytesWritten()
	expected := s.accepted - s.stageUsed
	if before != expected {
		return fmt.Errorf("unexpected_offset:%d", before)
	}

	flushed := s.stageUsed
	start := time.Now()
	if rc := s.updater.WriteChunk(s.stage[:flushed]); rc != 0 {
		return fmt.Errorf("write_chunk:%d", rc)
	}
	after := s.updater.BytesWritten()
	s.stageUsed = 0

	dt := time.Since(start)
	if force || seq == 0 || (seq%32) == 31 || dt >= 2*time.Millisecond {
		println(
			"[fabric]", "xfer_sink_flush",
			"seq", u32s(seq),
			"stage_n", u32s(flushed),
			"bytes_before", u32s(before),
			"bytes_after", u32s(after),
			"dt_us", u32s(uint32(dt/time.Microsecond)),
		)
	}

	return nil
}

func (s *rp2350TransferSink) WriteChunk(seq, off uint32, data []byte) error {
	if s.accepted != off {
		return fmt.Errorf("unexpected_offset:%d", s.accepted)
	}

	remaining := data
	for len(remaining) > 0 {
		if s.stageUsed == uint32(len(s.stage)) {
			if err := s.flushStage(seq, false); err != nil {
				return err
			}
		}

		n := copy(s.stage[s.stageUsed:], remaining)
		s.stageUsed += uint32(n)
		s.accepted += uint32(n)
		remaining = remaining[n:]

		if s.stageUsed == uint32(len(s.stage)) {
			if err := s.flushStage(seq, false); err != nil {
				return err
			}
		}
	}

	if seq == 0 || (seq%32) == 31 {
		println(
			"[fabric]", "xfer_sink_stage",
			"seq", u32s(seq),
			"off", u32s(off),
			"n", u32s(uint32(len(data))),
			"stage_used", u32s(s.stageUsed),
			"accepted", u32s(s.accepted),
		)
	}

	return nil
}

func (s *rp2350TransferSink) Commit() (transferInfo, error) {
	if err := s.flushStage(0, true); err != nil {
		return transferInfo{}, err
	}
	if rc := s.updater.FlushFinal(); rc != 0 {
		return transferInfo{}, fmt.Errorf("flush_final:%d", rc)
	}
	return transferInfo{
		BytesWritten: s.updater.BytesWritten(),
		SlotXIPAddr:  s.updater.SlotStorageAddr(),
	}, nil
}

func (s *rp2350TransferSink) Apply() error {
	if rc := s.updater.RebootIntoSlot(); rc != 0 {
		return fmt.Errorf("reboot:%d", rc)
	}
	return nil
}

func (s *rp2350TransferSink) Abort(reason string) error {
	_ = reason
	s.stageUsed = 0
	return nil
}

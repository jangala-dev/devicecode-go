//go:build tinygo && rp2350 && flash_unsafe

// Direct abupdate flashing on incoming transfers. Gated behind the
// `flash_unsafe` build tag because this path flashes raw bytes without
// signed-image verification (signature verify and staging belong in
// fabric-update's receiver). Use only for development or hardware bring-up
// where unsigned images are acceptable. The default fabric-protocol build
// rejects transfers at xfer_begin — see transfer_sink_rp2350.go.

package fabric

import (
	"fmt"
	"time"

	"pico2-a-b/abupdate"
)

const stageSize = 4096

type transferSinkImpl struct {
	updater *abupdate.Updater

	// Stage verified transfer bytes in protocol code so flash writes happen
	// in larger batches instead of directly on every UART chunk.
	stage      [stageSize]byte
	stageUsed  uint32
	accepted   uint32
	chunksSeen uint32
}

// beginTransfer creates an MCU-side sink for incoming firmware bytes. In the
// fabric-protocol baseline this accepts any well-formed transfer; image-format
// validation (kind/format/target/signature) is the receiver's job in
// fabric-update.
func beginTransfer(meta transferMeta) (transferSink, error) {
	var updater abupdate.Updater
	if rc := updater.Init(); rc != 0 {
		return nil, fmt.Errorf("updater_init:%d", rc)
	}
	if rc := updater.BeginUpdate(meta.Size); rc != 0 {
		return nil, fmt.Errorf("begin_update:%d", rc)
	}

	return &transferSinkImpl{updater: &updater}, nil
}

func (s *transferSinkImpl) flushStage(force bool) error {
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
	chunk := s.chunksSeen
	if force || chunk == 0 || (chunk%32) == 31 || dt >= 2*time.Millisecond {
		println(
			"[fabric]", "xfer_sink_flush",
			"chunk", u32s(chunk),
			"stage_n", u32s(flushed),
			"bytes_before", u32s(before),
			"bytes_after", u32s(after),
			"dt_us", u32s(uint32(dt/time.Microsecond)),
		)
	}

	return nil
}

func (s *transferSinkImpl) WriteChunk(off uint32, data []byte) error {
	if s.accepted != off {
		return fmt.Errorf("unexpected_offset:%d", s.accepted)
	}

	remaining := data
	for len(remaining) > 0 {
		if s.stageUsed == uint32(len(s.stage)) {
			if err := s.flushStage(false); err != nil {
				return err
			}
		}

		n := copy(s.stage[s.stageUsed:], remaining)
		s.stageUsed += uint32(n)
		s.accepted += uint32(n)
		remaining = remaining[n:]

		if s.stageUsed == uint32(len(s.stage)) {
			if err := s.flushStage(false); err != nil {
				return err
			}
		}
	}

	chunk := s.chunksSeen
	if chunk == 0 || (chunk%32) == 31 {
		println(
			"[fabric]", "xfer_sink_stage",
			"chunk", u32s(chunk),
			"off", u32s(off),
			"n", u32s(uint32(len(data))),
			"stage_used", u32s(s.stageUsed),
			"accepted", u32s(s.accepted),
		)
	}
	s.chunksSeen++

	return nil
}

func (s *transferSinkImpl) Commit() (transferInfo, error) {
	if err := s.flushStage(true); err != nil {
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

func (s *transferSinkImpl) Apply() error {
	if rc := s.updater.RebootIntoSlot(); rc != 0 {
		return fmt.Errorf("reboot:%d", rc)
	}
	return nil
}

func (s *transferSinkImpl) Abort(reason string) error {
	_ = reason
	s.stageUsed = 0
	return nil
}

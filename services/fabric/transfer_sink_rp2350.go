//go:build tinygo && rp2350

package fabric

import (
	"errors"
	"fmt"

	"ab-bringup/abupdate"
)

var errTransferUnsupported = errors.New("unsupported")

type rp2350TransferFactory struct{}

type rp2350TransferSink struct {
	updater *abupdate.Updater
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

func (s *rp2350TransferSink) WriteChunk(seq, off uint32, data []byte) error {
	_ = seq
	if got := s.updater.BytesWritten(); got != off {
		return fmt.Errorf("unexpected_offset:%d", got)
	}
	if rc := s.updater.WriteChunk(data); rc != 0 {
		return fmt.Errorf("write_chunk:%d", rc)
	}
	return nil
}

func (s *rp2350TransferSink) Commit() (transferInfo, error) {
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
	return nil
}

//go:build tinygo && rp2350

package updater

import (
	"errors"

	"pico2-a-b/abupdate"
)

// sharedUpdater is the package-level abupdate instance. It must persist
// across the staging path (which writes the staged image into the
// inactive slot via the abupdateSink) and the applier path (which
// reboots into that slot). One device = one inactive slot, so a
// singleton is fine.
var (
	sharedUpdater     abupdate.Updater
	sharedUpdaterInit bool
)

func ensureUpdaterInited() (*abupdate.Updater, error) {
	if !sharedUpdaterInit {
		if rc := sharedUpdater.Init(); rc != 0 {
			return nil, errFromRC("updater_init", rc)
		}
		sharedUpdaterInit = true
	}
	return &sharedUpdater, nil
}

// abupdateSink streams verified payload bytes straight into the
// inactive A/B slot via abupdate.WriteChunk. Commit() pads + writes
// the final partial page; Abort() leaves the slot in its current
// state (the next BeginUpdate erases sectors lazily as the next image
// is written).
type abupdateSink struct {
	u      *abupdate.Updater
	closed bool
}

func (s *abupdateSink) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("abupdate_sink: closed")
	}
	if rc := s.u.WriteChunk(p); rc != 0 {
		return 0, errFromRC("write_chunk", rc)
	}
	return len(p), nil
}

func (s *abupdateSink) Commit() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if rc := s.u.FlushFinal(); rc != 0 {
		return errFromRC("flush_final", rc)
	}
	return nil
}

func (s *abupdateSink) Abort() error {
	s.closed = true
	return nil
}

// newSlotSink resolves the inactive slot, calls BeginUpdate(totalSize)
// so abupdate knows when to stop erasing, and hands back a sink that
// streams into flash. The staging path creates one of these per Verify
// call.
func newSlotSink(totalSize uint32) (SlotSink, error) {
	u, err := ensureUpdaterInited()
	if err != nil {
		return nil, err
	}
	if rc := u.BeginUpdate(totalSize); rc != 0 {
		return nil, errFromRC("begin_update", rc)
	}
	return &abupdateSink{u: u}, nil
}

func errFromRC(op string, rc int32) error {
	return &rcError{op: op, rc: rc}
}

type rcError struct {
	op string
	rc int32
}

func (e *rcError) Error() string {
	return e.op + ":" + i32s(e.rc)
}

func i32s(v int32) string {
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	var buf [12]byte
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

package fabric

import (
	"encoding/base64"
	"encoding/json"
	"runtime"
	"strings"
	"time"

	"devicecode-go/x/strconvx"
	"devicecode-go/x/xxhash"
)

const postTransferDoneSettle = 250 * time.Millisecond
const transferProgressLogEvery = 32

// transferMeta captures xfer_begin contents. The required Lua wire shape is
// {xfer_id, size, checksum}; meta is optional but source-used (transfer_mgr
// passes it through to the receiver, where meta.receiver names a local
// endpoint to call after xfer_commit and before xfer_done). Preserve meta
// as an opaque blob — interpretation lives in fabric-update.
type transferMeta struct {
	ID       string
	Size     uint32
	Checksum string // xxHash32 hex (8 lower-case hex chars), no algorithm field
	Meta     json.RawMessage
}

// transferInfo is internal-only state returned by the sink on Commit. It is
// no longer wire-visible — xfer_done carries only xfer_id in the canonical
// schema; size/checksum reconciliation lives on xfer_commit.
type transferInfo struct {
	BytesWritten uint32
	SlotXIPAddr  uint32
}

// transferSink is the firmware-side write target for an incoming transfer.
// WriteChunk receives bytes at the given byte offset (matching xfer_chunk's
// canonical wire fields). No sequence number is passed — the caller has
// already validated offset against expected progress.
type transferSink interface {
	WriteChunk(offset uint32, data []byte) error
	Commit() (transferInfo, error)
	Apply() error
	Abort(reason string) error
}

type incomingTransfer struct {
	meta         transferMeta
	sink         transferSink
	bytesWritten uint32
	chunksSeen   uint32
	hasher       *xxhash.Hasher
	// deadline is the idle-chunk watchdog: bumped on every accepted chunk
	// and on initial xfer_begin. checkTransferTimeout fires if now > deadline.
	// Mirrors transfer_mgr.lua: `active.deadline = runtime.now() + phase_timeout`.
	deadline time.Time
}

func lowerHex(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func u32s(v uint32) string {
	return strconvx.Itoa(int(v))
}

func (s *session) sendTransferReady(id string) bool {
	return s.sendControl(marshal(protoXferReady{
		Type:   msgXferReady,
		XferID: id,
	}))
}

func (s *session) sendTransferNeed(id string, next uint32) bool {
	return s.sendControl(marshal(protoXferNeed{
		Type:   msgXferNeed,
		XferID: id,
		Next:   next,
	}))
}

func (s *session) sendTransferDone(id string) bool {
	return s.sendControl(marshal(protoXferDone{
		Type:   msgXferDone,
		XferID: id,
	}))
}

func (s *session) sendTransferAbort(id, reason string) bool {
	return s.sendControl(marshal(protoXferAbort{
		Type:   msgXferAbort,
		XferID: id,
		Err:    reason,
	}))
}

func (s *session) clearTransfer() *incomingTransfer {
	cur := s.incomingTransfer
	s.incomingTransfer = nil
	return cur
}

func (s *session) abortTransfer(reason string) {
	cur := s.clearTransfer()
	if cur == nil {
		return
	}
	if err := cur.sink.Abort(reason); err != nil {
		s.logKV("transfer abort failed", "err", err.Error())
	}
}

// checkTransferTimeout enforces the idle-chunk watchdog. Fires once per
// drain tick from the session run loop; cheap when no transfer is active.
// On expiry both the local sink is aborted and an xfer_abort frame is sent
// to the peer (matching Lua transfer_mgr.lua's `clear_active('timeout')` +
// outbound xfer_abort).
func (s *session) checkTransferTimeout(now time.Time) {
	cur := s.incomingTransfer
	if cur == nil {
		return
	}
	if !now.After(cur.deadline) {
		return
	}
	id := cur.meta.ID
	println("[fabric]", "sid", s.localSID, "xfer_phase_timeout",
		"id", id, "phase_s", u32s(uint32(s.cfg.PhaseTimeout/time.Second)))
	s.abortTransfer("timeout")
	s.sendTransferAbort(id, "timeout")
}

func validateTransferBegin(msg *protoXferBegin) (transferMeta, string) {
	if msg.XferID == "" {
		return transferMeta{}, "xfer_begin.xfer_id"
	}
	if msg.Size == 0 {
		return transferMeta{}, "xfer_begin.size"
	}
	if msg.Checksum == "" {
		return transferMeta{}, "xfer_begin.checksum"
	}
	return transferMeta{
		ID:       msg.XferID,
		Size:     msg.Size,
		Checksum: lowerHex(msg.Checksum),
		Meta:     append(json.RawMessage(nil), msg.Meta...),
	}, ""
}

func (s *session) onTransferBegin(msg *protoXferBegin) {
	meta, errStr := validateTransferBegin(msg)
	if errStr != "" {
		if msg.XferID != "" {
			s.sendTransferAbort(msg.XferID, "bad_message: "+errStr)
		}
		s.logKV("xfer_begin dropped", "err", errStr)
		return
	}
	if s.incomingTransfer != nil {
		s.sendTransferAbort(meta.ID, "busy")
		return
	}
	beginFn := s.beginTransfer
	if beginFn == nil {
		beginFn = beginTransfer
	}
	sink, err := beginFn(meta)
	if err != nil {
		s.sendTransferAbort(meta.ID, err.Error())
		return
	}
	s.incomingTransfer = &incomingTransfer{
		meta:     meta,
		sink:     sink,
		hasher:   xxhash.New(0),
		deadline: time.Now().Add(s.cfg.PhaseTimeout),
	}
	println(
		"[fabric]", "sid", s.localSID,
		"xfer_begin accepted",
		"id", meta.ID,
		"size", u32s(meta.Size),
		"checksum", meta.Checksum,
	)
	s.sendTransferReady(meta.ID)
}

func (s *session) onTransferChunk(msg *protoXferChunk) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.XferID {
		s.logKV("xfer_chunk dropped", "id", msg.XferID)
		return
	}
	// Lua transfer_mgr.lua aborts and clears the active transfer on any
	// chunk-level fault (unexpected offset, decode failure, size mismatch).
	// Match that — do not send xfer_need + keep alive.
	id := cur.meta.ID
	if msg.Offset != cur.bytesWritten {
		println("[fabric]", "sid", s.localSID, "xfer_chunk aborted",
			"id", id, "err", "unexpected_offset",
			"off", u32s(msg.Offset), "want_off", u32s(cur.bytesWritten))
		s.abortTransfer("unexpected_offset")
		s.sendTransferAbort(id, "unexpected_offset")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(msg.Data)
	if err != nil {
		println("[fabric]", "sid", s.localSID, "xfer_chunk aborted",
			"id", id, "err", "decode_failed",
			"off", u32s(msg.Offset), "data_len", u32s(uint32(len(msg.Data))))
		s.abortTransfer("decode_failed")
		s.sendTransferAbort(id, "decode_failed")
		return
	}
	if len(raw) == 0 {
		println("[fabric]", "sid", s.localSID, "xfer_chunk aborted",
			"id", id, "err", "empty_chunk", "off", u32s(msg.Offset))
		s.abortTransfer("empty_chunk")
		s.sendTransferAbort(id, "empty_chunk")
		return
	}
	if cur.bytesWritten+uint32(len(raw)) > cur.meta.Size {
		println("[fabric]", "sid", s.localSID, "xfer_chunk aborted",
			"id", id, "err", "size_overflow",
			"bytes_written", u32s(cur.bytesWritten),
			"raw_len", u32s(uint32(len(raw))),
			"total", u32s(cur.meta.Size))
		s.abortTransfer("size_overflow")
		s.sendTransferAbort(id, "size_overflow")
		return
	}
	if err := cur.sink.WriteChunk(msg.Offset, raw); err != nil {
		reason := err.Error()
		s.logKV("transfer write failed", "err", reason)
		s.abortTransfer(reason)
		s.sendTransferAbort(id, reason)
		return
	}
	_, _ = cur.hasher.Write(raw)
	cur.bytesWritten += uint32(len(raw))
	cur.chunksSeen++
	cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
	if cur.chunksSeen == 1 || (cur.chunksSeen%transferProgressLogEvery) == 0 {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_chunk accepted",
			"id", cur.meta.ID,
			"off", u32s(msg.Offset),
			"data_len", u32s(uint32(len(raw))),
			"bytes_written", u32s(cur.bytesWritten),
		)
	}
	raw = nil
	// Forced GC after each absorbed chunk eliminates firmware-transfer byte
	// drops on the safe-window allocator. Do NOT remove this without
	// reproducing the regression in firmware-mono/docs/old/FABRIC_TRANSFER_FIX.md.
	runtime.GC()
	s.sendTransferNeed(cur.meta.ID, cur.bytesWritten)
}

func (s *session) onTransferCommit(msg *protoXferCommit) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.XferID {
		s.logKV("xfer_commit dropped", "id", msg.XferID)
		return
	}
	id := cur.meta.ID
	if msg.Size != cur.meta.Size || cur.bytesWritten != cur.meta.Size {
		println("[fabric]", "sid", s.localSID, "xfer_commit failed",
			"id", id, "err", "size_mismatch",
			"bytes_written", u32s(cur.bytesWritten),
			"msg_size", u32s(msg.Size), "meta_size", u32s(cur.meta.Size))
		s.abortTransfer("size_mismatch")
		s.sendTransferAbort(id, "size_mismatch")
		return
	}
	streamedHex := xxhashHex(cur.hasher.Sum32())
	commitChecksum := lowerHex(msg.Checksum)
	if commitChecksum != cur.meta.Checksum || streamedHex != cur.meta.Checksum {
		println("[fabric]", "sid", s.localSID, "xfer_commit failed",
			"id", id, "err", "checksum_mismatch",
			"begin", cur.meta.Checksum,
			"commit", commitChecksum,
			"streamed", streamedHex,
		)
		s.abortTransfer("checksum_mismatch")
		s.sendTransferAbort(id, "checksum_mismatch")
		return
	}
	info, err := cur.sink.Commit()
	if err != nil {
		s.logKV("transfer commit failed", "err", err.Error())
		reason := err.Error()
		s.abortTransfer(reason)
		s.sendTransferAbort(id, reason)
		return
	}
	sink := cur.sink
	s.clearTransfer()
	println(
		"[fabric]", "sid", s.localSID,
		"xfer_commit accepted",
		"id", id,
		"bytes_written", u32s(info.BytesWritten),
	)
	if !s.sendTransferDone(id) {
		return
	}
	time.Sleep(postTransferDoneSettle)
	if err := sink.Apply(); err != nil {
		s.logKV("transfer apply failed", "err", err.Error())
		return
	}
	println("[fabric]", "sid", s.localSID, "transfer apply ok", "id", id)
}

func (s *session) onTransferAbort(msg *protoXferAbort) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.XferID {
		s.logKV("xfer_abort dropped", "id", msg.XferID)
		return
	}
	reason := msg.Err
	if reason == "" {
		reason = "remote_abort"
	}
	println("[fabric]", "sid", s.localSID, "xfer_abort received", "id", cur.meta.ID, "reason", reason)
	s.abortTransfer(reason)
}

// xxhashHex formats a uint32 xxHash32 digest as 8 lower-case hex characters,
// matching the wire format used by the Lua reference's M.digest_hex.
func xxhashHex(v uint32) string {
	const digits = "0123456789abcdef"
	var buf [8]byte
	for i := 7; i >= 0; i-- {
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

package fabric

import (
	"encoding/base64"
	"encoding/json"
	"runtime"
	"strings"
	"time"

	"devicecode-go/services/otadiag"
	"devicecode-go/services/updater"
	"devicecode-go/x/strconvx"
	"devicecode-go/x/xxhash"
)

const transferTargetUpdaterMain = "updater/main"
const transferIdleRetryLimit = 3
const transferCorruptRetryLimit = 3
const completedTransferCacheLimit = 4
const transferMemSampleStride = 64 * 1024

// transferMeta captures xfer_begin contents. The transfer target is explicit
// on the wire; firmware update uses target="updater/main". meta remains opaque
// and informational to Fabric.
type transferMeta struct {
	ID        string
	Target    string
	Size      uint32
	DigestAlg string
	Digest    string // xxHash32 hex (8 lower-case hex chars), seed 0
	Meta      json.RawMessage
}

// transferInfo is internal-only state returned by the sink on Commit. It is
// no longer wire-visible — xfer_done carries only xfer_id in the canonical
// schema; size/digest reconciliation lives on xfer_commit.
type transferInfo struct {
	BytesWritten uint32
	SlotXIPAddr  uint32
	Generation   uint64
}

// transferSink is the firmware-side write target for an incoming transfer.
// WriteChunk receives bytes at the given byte offset (matching xfer_chunk's
// canonical wire fields). No sequence number is passed — the caller has
// already validated offset against expected progress.
//
// Bytes() returns the committed payload bytes for target invocation.
// Only valid after Commit() has succeeded. May return nil if the sink
// streamed the bytes elsewhere (e.g. the RP2350 sink writes directly to
// flash and doesn't keep a RAM copy); updater/main consumes that staged
// stream from the updater package.
type transferSink interface {
	WriteChunk(offset uint32, data []byte) error
	Commit() (transferInfo, error)
	Apply() error
	Abort(reason string) error
	Bytes() []byte
}

type incomingTransfer struct {
	meta                   transferMeta
	sink                   transferSink
	bytesWritten           uint32
	chunksSeen             uint32
	hasher                 *xxhash.Hasher
	idleRetries            uint8
	corruptRetryOffset     uint32
	corruptRetriesAtOffset uint8
	// deadline is the idle-chunk watchdog: bumped on every accepted chunk
	// and on initial xfer_begin. checkTransferTimeout fires if now > deadline.
	// Mirrors transfer_mgr.lua: `active.deadline = runtime.now() + phase_timeout`.
	deadline time.Time
}

type completedTransfer struct {
	meta transferMeta
}

func sameTransferTuple(a, b transferMeta) bool {
	return a.ID == b.ID &&
		a.Target == b.Target &&
		a.Size == b.Size &&
		a.DigestAlg == b.DigestAlg &&
		a.Digest == b.Digest
}

func lowerHex(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func canonicalXXHash32Hex(s string) (string, bool) {
	digest := lowerHex(s)
	return digest, s == digest && validXXHash32Hex(digest)
}

func validXXHash32Hex(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func u32s(v uint32) string {
	return strconvx.Itoa(int(v))
}

func decodeChunkData(encoded string) ([]byte, string) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, "invalid_chunk_encoding"
	}
	if base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return nil, "invalid_chunk_encoding"
	}
	return raw, ""
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
	otadiag.Event("[fabric-xfer]", "abort_local", cur.meta.ID, otadiag.KV("reason", reason))
	otadiag.StopUpdateWindow(reason)
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
	if cur.idleRetries < transferIdleRetryLimit {
		cur.idleRetries++
		cur.deadline = now.Add(s.cfg.PhaseTimeout)
		s.logKV("transfer idle retry", "offset", u32s(cur.bytesWritten))
		s.sendTransferNeed(cur.meta.ID, cur.bytesWritten)
		return
	}
	id := cur.meta.ID
	s.abortTransfer("timeout")
	abortOK := s.sendTransferAbort(id, "timeout")
	otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", "timeout"), otadiag.KV("ok", abortOK))
}

func (s *session) retryCorruptTransferFrame(reason string) bool {
	cur := s.incomingTransfer
	if cur == nil {
		return false
	}
	if cur.corruptRetryOffset != cur.bytesWritten {
		cur.corruptRetryOffset = cur.bytesWritten
		cur.corruptRetriesAtOffset = 0
	}
	if cur.corruptRetriesAtOffset >= transferCorruptRetryLimit {
		id := cur.meta.ID
		s.abortTransfer(reason)
		abortOK := s.sendTransferAbort(id, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
		return false
	}
	cur.corruptRetriesAtOffset++
	needOK := s.sendTransferNeed(cur.meta.ID, cur.bytesWritten)
	otadiag.Event(
		"[fabric-xfer]", "need_tx", cur.meta.ID,
		otadiag.KV("next", cur.bytesWritten),
		otadiag.KV("ok", needOK),
		otadiag.KV("retry", true),
		otadiag.KV("reason", reason),
	)
	cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
	return true
}

func (s *session) completedTransferFor(id string) (transferMeta, bool) {
	for _, rec := range s.completedTransfers {
		if rec.meta.ID == id {
			return rec.meta, true
		}
	}
	return transferMeta{}, false
}

func (s *session) recordCompletedTransfer(meta transferMeta) {
	for i, rec := range s.completedTransfers {
		if rec.meta.ID == meta.ID {
			s.completedTransfers = append(s.completedTransfers[:i], s.completedTransfers[i+1:]...)
			break
		}
	}
	s.completedTransfers = append(s.completedTransfers, completedTransfer{meta: meta})
	if len(s.completedTransfers) > completedTransferCacheLimit {
		copy(s.completedTransfers, s.completedTransfers[len(s.completedTransfers)-completedTransferCacheLimit:])
		s.completedTransfers = s.completedTransfers[:completedTransferCacheLimit]
	}
}

func (s *session) clearCompletedTransfers() {
	s.completedTransfers = nil
}

func validateTransferBegin(msg *protoXferBegin) (transferMeta, string) {
	if msg.XferID == "" {
		return transferMeta{}, "xfer_begin.xfer_id"
	}
	if msg.Target == "" {
		return transferMeta{}, "missing_target"
	}
	if msg.Target != transferTargetUpdaterMain {
		return transferMeta{}, "unsupported_target"
	}
	if msg.Size == 0 {
		return transferMeta{}, "xfer_begin.size"
	}
	if msg.DigestAlg != digestAlg {
		return transferMeta{}, "unsupported_digest_alg"
	}
	digest, ok := canonicalXXHash32Hex(msg.Digest)
	if !ok {
		return transferMeta{}, "invalid_digest"
	}
	return transferMeta{
		ID:        msg.XferID,
		Target:    msg.Target,
		Size:      msg.Size,
		DigestAlg: msg.DigestAlg,
		Digest:    digest,
		Meta:      append(json.RawMessage(nil), msg.Meta...),
	}, ""
}

func (s *session) onTransferBegin(msg *protoXferBegin) {
	s.extendTransferQuiet("xfer_begin_rx", transferPrepareQuiet)
	otadiag.SetActiveXfer(msg.XferID)
	otadiag.Event(
		"[fabric-xfer]", "begin_rx", msg.XferID,
		otadiag.KV("target", msg.Target),
		otadiag.KV("size", msg.Size),
		otadiag.KV("digest_alg", msg.DigestAlg),
		otadiag.KV("digest", msg.Digest),
		otadiag.KV("meta_len", len(msg.Meta)),
	)
	meta, errStr := validateTransferBegin(msg)
	if errStr != "" {
		if msg.XferID != "" {
			abortOK := s.sendTransferAbort(msg.XferID, "bad_message: "+errStr)
			otadiag.Event(
				"[fabric-xfer]", "begin_reject", msg.XferID,
				otadiag.KV("reason", "bad_message:"+errStr),
				otadiag.KV("abort_tx", abortOK),
			)
		} else {
			otadiag.Event(
				"[fabric-xfer]", "begin_reject", msg.XferID,
				otadiag.KV("reason", "bad_message:"+errStr),
				otadiag.KV("abort_tx", false),
			)
		}
		otadiag.StopUpdateWindow("begin_reject")
		s.logKV("xfer_begin dropped", "err", errStr)
		return
	}
	otadiag.Event(
		"[fabric-xfer]", "begin_validate_ok", meta.ID,
		otadiag.KV("target", meta.Target),
	)
	s.markRx()
	now := time.Now()
	if s.incomingTransfer != nil {
		cur := s.incomingTransfer
		if sameTransferTuple(cur.meta, meta) {
			s.logKV("xfer_begin duplicate", "id", meta.ID)
			readyOK := s.sendTransferReady(meta.ID)
			otadiag.Event("[fabric-xfer]", "ready_tx", meta.ID, otadiag.KV("ok", readyOK), otadiag.KV("duplicate", true))
			if readyOK {
				needOK := s.sendTransferNeed(meta.ID, cur.bytesWritten)
				otadiag.Event("[fabric-xfer]", "need_tx", meta.ID, otadiag.KV("next", cur.bytesWritten), otadiag.KV("ok", needOK), otadiag.KV("duplicate", true))
			}
			cur.deadline = now.Add(s.cfg.PhaseTimeout)
			return
		}
		reason := "busy"
		if cur.meta.ID == meta.ID {
			reason = "conflicting_transfer"
		}
		abortOK := s.sendTransferAbort(meta.ID, reason)
		otadiag.Event(
			"[fabric-xfer]", "begin_reject", meta.ID,
			otadiag.KV("reason", reason),
			otadiag.KV("active_xfer", cur.meta.ID),
			otadiag.KV("abort_tx", abortOK),
		)
		otadiag.StopUpdateWindow("begin_reject")
		return
	}
	if done, ok := s.completedTransferFor(meta.ID); ok {
		if sameTransferTuple(done, meta) {
			doneOK := s.sendTransferDone(meta.ID)
			otadiag.Event("[fabric-xfer]", "begin_duplicate_done", meta.ID, otadiag.KV("done_tx", doneOK))
			return
		}
		abortOK := s.sendTransferAbort(meta.ID, "conflicting_transfer")
		otadiag.Event("[fabric-xfer]", "begin_reject", meta.ID, otadiag.KV("reason", "conflicting_transfer"), otadiag.KV("abort_tx", abortOK))
		otadiag.StopUpdateWindow("begin_reject")
		return
	}
	beginFn := s.beginTransfer
	if beginFn == nil {
		beginFn = beginTransfer
	}
	beginStart := time.Now()
	otadiag.Event(
		"[fabric-xfer]", "begin_transfer_start", meta.ID,
		otadiag.KV("target", meta.Target),
		otadiag.KV("size", meta.Size),
	)
	sink, err := beginFn(meta)
	if err != nil {
		durMS := int(time.Since(beginStart) / time.Millisecond)
		abortOK := s.sendTransferAbort(meta.ID, err.Error())
		otadiag.Event(
			"[fabric-xfer]", "begin_transfer_error", meta.ID,
			otadiag.KV("err", err.Error()),
			otadiag.KV("dur_ms", durMS),
			otadiag.KV("abort_tx", abortOK),
		)
		otadiag.StopUpdateWindow("begin_transfer_error")
		return
	}
	otadiag.Event(
		"[fabric-xfer]", "begin_transfer_done", meta.ID,
		otadiag.KV("dur_ms", int(time.Since(beginStart)/time.Millisecond)),
	)
	s.incomingTransfer = &incomingTransfer{
		meta:     meta,
		sink:     sink,
		hasher:   xxhash.New(0),
		deadline: now.Add(s.cfg.PhaseTimeout),
	}
	readyOK := s.sendTransferReady(meta.ID)
	otadiag.Event("[fabric-xfer]", "ready_tx", meta.ID, otadiag.KV("ok", readyOK))
	if readyOK {
		needOK := s.sendTransferNeed(meta.ID, 0)
		otadiag.Event("[fabric-xfer]", "need_tx", meta.ID, otadiag.KV("next", 0), otadiag.KV("ok", needOK))
	} else {
		otadiag.Event("[fabric-xfer]", "need_tx", meta.ID, otadiag.KV("next", 0), otadiag.KV("ok", false), otadiag.KV("skipped", "ready_failed"))
	}
}

func (s *session) onTransferChunk(msg *protoXferChunk) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.XferID {
		s.logKV("xfer_chunk dropped", "id", msg.XferID)
		return
	}
	id := cur.meta.ID
	otadiag.Event(
		"[fabric-xfer]", "chunk_rx", id,
		otadiag.KV("offset", u32s(msg.Offset)),
		otadiag.KV("expected", u32s(cur.bytesWritten)),
		otadiag.KV("encoded_len", strconvx.Itoa(len(msg.Data))),
	)
	if msg.Offset < cur.bytesWritten {
		s.markRx()
		needOK := s.sendTransferNeed(id, cur.bytesWritten)
		otadiag.Event(
			"[fabric-xfer]", "chunk_stale_offset", id,
			otadiag.KV("offset", u32s(msg.Offset)),
			otadiag.KV("expected", u32s(cur.bytesWritten)),
			otadiag.KV("need_tx", needOK),
		)
		cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
		return
	}
	if msg.Offset > cur.bytesWritten {
		s.markRx()
		needOK := s.sendTransferNeed(id, cur.bytesWritten)
		otadiag.Event(
			"[fabric-xfer]", "chunk_future_offset", id,
			otadiag.KV("offset", u32s(msg.Offset)),
			otadiag.KV("expected", u32s(cur.bytesWritten)),
			otadiag.KV("need_tx", needOK),
		)
		return
	}
	decodeStart := time.Now()
	raw, errStr := decodeChunkData(msg.Data)
	if errStr != "" {
		otadiag.Event(
			"[fabric-xfer]", "chunk_decode_done", id,
			otadiag.KV("ok", false),
			otadiag.KV("reason", errStr),
			otadiag.KV("dur_ms", int(time.Since(decodeStart)/time.Millisecond)),
		)
		s.logKV("xfer_chunk decode retry", "err", errStr)
		s.retryCorruptTransferFrame(errStr)
		return
	}
	otadiag.Event(
		"[fabric-xfer]", "chunk_decode_done", id,
		otadiag.KV("ok", true),
		otadiag.KV("raw_len", strconvx.Itoa(len(raw))),
		otadiag.KV("dur_ms", int(time.Since(decodeStart)/time.Millisecond)),
	)
	if len(raw) == 0 {
		s.abortTransfer("empty_chunk")
		abortOK := s.sendTransferAbort(id, "empty_chunk")
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", "empty_chunk"), otadiag.KV("ok", abortOK))
		return
	}
	if cur.bytesWritten+uint32(len(raw)) > cur.meta.Size {
		reason := "size_too_large"
		otadiag.Event(
			"[fabric-xfer]", "chunk_size_overflow", id,
			otadiag.KV("offset", u32s(msg.Offset)),
			otadiag.KV("raw_len", strconvx.Itoa(len(raw))),
			otadiag.KV("size", u32s(cur.meta.Size)),
		)
		s.abortTransfer(reason)
		abortOK := s.sendTransferAbort(id, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
		return
	}
	// Per-chunk integrity is required by the current MCU contract.
	// JSON parsing alone misses single-byte UART corruption inside the
	// base64url data string: the bytes still decode, just to the wrong
	// values. On mismatch we ask the sender to resume at the current
	// byte offset instead of clearing the transfer.
	digestStart := time.Now()
	want, ok := canonicalXXHash32Hex(msg.ChunkDigest)
	if !ok {
		otadiag.Event(
			"[fabric-xfer]", "chunk_digest_done", id,
			otadiag.KV("ok", false),
			otadiag.KV("reason", "bad_message"),
			otadiag.KV("offset", u32s(msg.Offset)),
			otadiag.KV("digest_len", strconvx.Itoa(len(msg.ChunkDigest))),
			otadiag.KV("data_len", strconvx.Itoa(len(msg.Data))),
			otadiag.KV("dur_ms", int(time.Since(digestStart)/time.Millisecond)),
		)
		s.retryCorruptTransferFrame("bad_message")
		return
	}
	got := xxhashHex(xxhash.Sum32(raw, 0))
	if got != want {
		otadiag.Event(
			"[fabric-xfer]", "chunk_digest_done", id,
			otadiag.KV("ok", false),
			otadiag.KV("reason", "chunk_digest_mismatch"),
			otadiag.KV("offset", u32s(msg.Offset)),
			otadiag.KV("dur_ms", int(time.Since(digestStart)/time.Millisecond)),
		)
		s.retryCorruptTransferFrame("chunk_digest_mismatch")
		return
	}
	otadiag.Event(
		"[fabric-xfer]", "chunk_digest_done", id,
		otadiag.KV("ok", true),
		otadiag.KV("dur_ms", int(time.Since(digestStart)/time.Millisecond)),
	)
	s.markRx()
	writeStart := time.Now()
	otadiag.Event(
		"[fabric-xfer]", "sink_write_start", id,
		otadiag.KV("offset", u32s(msg.Offset)),
		otadiag.KV("raw_len", strconvx.Itoa(len(raw))),
	)
	if err := cur.sink.WriteChunk(msg.Offset, raw); err != nil {
		reason := err.Error()
		otadiag.Event(
			"[fabric-xfer]", "sink_write_error", id,
			otadiag.KV("reason", reason),
			otadiag.KV("dur_ms", int(time.Since(writeStart)/time.Millisecond)),
		)
		s.logKV("transfer write failed", "err", reason)
		s.abortTransfer(reason)
		abortOK := s.sendTransferAbort(id, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
		return
	}
	_, _ = cur.hasher.Write(raw)
	cur.bytesWritten += uint32(len(raw))
	cur.chunksSeen++
	cur.idleRetries = 0
	cur.corruptRetryOffset = cur.bytesWritten
	cur.corruptRetriesAtOffset = 0
	cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
	otadiag.Event(
		"[fabric-xfer]", "sink_write_done", id,
		otadiag.KV("dur_ms", int(time.Since(writeStart)/time.Millisecond)),
		otadiag.KV("next", u32s(cur.bytesWritten)),
	)
	raw = nil
	// Keep transfer memory bounded on TinyGo. The receiver allocates while
	// unmarshalling JSON and decoding base64 chunks; without regular collection
	// long updates can run out of heap before commit.
	gcStart := time.Now()
	otadiag.Event("[fabric-xfer]", "gc_start", id, otadiag.KV("next", u32s(cur.bytesWritten)))
	runtime.GC()
	otadiag.Event(
		"[fabric-xfer]", "gc_done", id,
		otadiag.KV("dur_ms", int(time.Since(gcStart)/time.Millisecond)),
		otadiag.KV("next", u32s(cur.bytesWritten)),
	)
	needOK := s.sendTransferNeed(cur.meta.ID, cur.bytesWritten)
	otadiag.Event(
		"[fabric-xfer]", "need_tx", cur.meta.ID,
		otadiag.KV("next", cur.bytesWritten),
		otadiag.KV("ok", needOK),
		otadiag.KV("accepted", true),
	)
	if cur.bytesWritten != 0 && cur.bytesWritten%transferMemSampleStride == 0 {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		otadiag.Event(
			"[fabric-xfer]", "transfer_mem_sample", cur.meta.ID,
			otadiag.KV("next", cur.bytesWritten),
			otadiag.KV("alloc", ms.Alloc),
			otadiag.KV("heap", ms.HeapSys),
		)
	}
}

func (s *session) onTransferCommit(msg *protoXferCommit) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.XferID {
		s.logKV("xfer_commit dropped", "id", msg.XferID)
		return
	}
	id := cur.meta.ID
	if msg.Size != cur.meta.Size || cur.bytesWritten != cur.meta.Size {
		reason := "short_transfer"
		s.abortTransfer(reason)
		abortOK := s.sendTransferAbort(id, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
		return
	}
	if msg.DigestAlg != digestAlg {
		s.abortTransfer("unsupported_digest_alg")
		abortOK := s.sendTransferAbort(id, "unsupported_digest_alg")
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", "unsupported_digest_alg"), otadiag.KV("ok", abortOK))
		return
	}
	commitDigest, ok := canonicalXXHash32Hex(msg.Digest)
	if !ok {
		s.abortTransfer("invalid_digest")
		abortOK := s.sendTransferAbort(id, "invalid_digest")
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", "invalid_digest"), otadiag.KV("ok", abortOK))
		return
	}
	streamedHex := xxhashHex(cur.hasher.Sum32())
	if commitDigest != cur.meta.Digest || streamedHex != cur.meta.Digest {
		s.abortTransfer("digest_mismatch")
		abortOK := s.sendTransferAbort(id, "digest_mismatch")
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", "digest_mismatch"), otadiag.KV("ok", abortOK))
		return
	}
	s.markRx()
	info, err := cur.sink.Commit()
	if err != nil {
		s.logKV("transfer commit failed", "err", err.Error())
		reason := err.Error()
		s.abortTransfer(reason)
		abortOK := s.sendTransferAbort(id, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
		return
	}
	sink := cur.sink
	meta := cur.meta
	s.extendTransferQuiet("xfer_commit_target", transferCompleteQuiet)
	s.clearTransfer()

	bytesPayload := sink.Bytes()
	ok, reason := s.invokeTransferTarget(meta, id, info, bytesPayload)
	if !ok {
		s.extendTransferQuiet("xfer_target_rejected", transferCompleteQuiet)
		abortOK := s.sendTransferAbort(id, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
		return
	}
	s.extendTransferQuiet("xfer_done", transferCompleteQuiet)
	s.recordCompletedTransfer(meta)
	doneOK := s.sendTransferDone(id)
	otadiag.Event("[fabric-xfer]", "done_tx", id, otadiag.KV("ok", doneOK))
	otadiag.StopUpdateWindow("transfer_done")
}

var targetCallTimeout = 5 * time.Second

// invokeTransferTarget calls the local updater staging RPC named by
// xfer_begin.target. The wire no longer carries raw/member receiver topics;
// target="updater/main" maps to an internal bus RPC owned by the updater
// service. The reply gates whether fabric sends xfer_done or xfer_abort.
func (s *session) invokeTransferTarget(meta transferMeta, xferID string, info transferInfo, artefact []byte) (bool, string) {
	if meta.Target != transferTargetUpdaterMain {
		return false, "unsupported_target"
	}
	payload := updater.StagePayload{
		LinkID:     s.linkID,
		XferID:     xferID,
		Generation: info.Generation,
		Target:     meta.Target,
		Size:       meta.Size,
		DigestAlg:  meta.DigestAlg,
		Digest:     meta.Digest,
		Meta:       meta.Meta,
		Artefact:   artefact,
	}
	msg := s.conn.NewMessage(updater.TopicStageRPC, payload, false)
	replySub := s.conn.Request(msg)
	defer s.conn.Unsubscribe(replySub)

	select {
	case rep, ok := <-replySub.Channel():
		if !ok || rep == nil {
			updater.CancelStreamedStage(xferID, info.Generation, "stage_no_reply")
			return false, "stage_no_reply"
		}
		ok, reason := decodeStageReply(rep.Payload)
		if !ok {
			updater.CancelStreamedStage(xferID, info.Generation, reason)
			return false, reason
		}
		return true, ""
	case <-time.After(targetCallTimeout):
		updater.CancelStreamedStage(xferID, info.Generation, "stage_timeout")
		return false, "stage_timeout"
	}
}

func decodeStageReply(payload any) (bool, string) {
	switch v := payload.(type) {
	case nil:
		return false, "stage_nil_payload"
	case updater.StageReply:
		if !v.OK {
			if v.Err == "" {
				return false, "stage_rejected"
			}
			return false, v.Err
		}
		return true, ""
	case *updater.StageReply:
		if v == nil {
			return false, "stage_nil_payload"
		}
		if !v.OK {
			if v.Err == "" {
				return false, "stage_rejected"
			}
			return false, v.Err
		}
		return true, ""
	case map[string]any:
		ok, _ := v["ok"].(bool)
		if !ok {
			err, _ := v["err"].(string)
			if err == "" {
				err = "stage_rejected"
			}
			return false, err
		}
		return true, ""
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return false, "stage_marshal_failed"
	}
	var probe struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return false, "stage_unmarshal_failed"
	}
	if !probe.OK {
		if probe.Err == "" {
			return false, "stage_rejected"
		}
		return false, probe.Err
	}
	return true, ""
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
	s.markRx()
	s.abortTransfer(reason)
	otadiag.StopUpdateWindow("remote_abort")
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

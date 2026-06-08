package fabric

import (
	"encoding/base64"
	"encoding/json"
	"runtime"
	"strings"
	"time"

	"devicecode-go/bus"
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
// The sink owns transfer bytes. Fabric never asks it for a whole-image
// []byte; after Commit succeeds the updater/main stage RPC consumes the
// committed streamed stage by xfer_id/generation.
type transferSink interface {
	WriteChunk(offset uint32, data []byte) error
	Commit() (transferInfo, error)
	Apply() error
	Abort(reason string) error
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
	pendingChunk           *pendingChunkWrite
	pendingCommit          *pendingTransferCommit
	// deadline is the idle-chunk watchdog: bumped on every accepted chunk
	// and on initial xfer_begin. checkTransferTimeout fires if now > deadline.
	// Mirrors transfer_mgr.lua: `active.deadline = runtime.now() + phase_timeout`.
	// While a chunk write is pending this also bounds the staging operation;
	// the Fabric session loop stays live and the next xfer_need is not sent
	// until the updater sink reports that the chunk has been accepted.
	deadline time.Time
}

type pendingChunkWrite struct {
	xferID   string
	offset   uint32
	data     []byte
	started  time.Time
	resultCh chan error
}

type pendingTransferCommit struct {
	xferID   string
	started  time.Time
	resultCh chan transferCommitResult
}

type transferCommitResult struct {
	info transferInfo
	err  error
}

type completedTransfer struct {
	meta transferMeta
}

type pendingTargetCall struct {
	xferID   string
	meta     transferMeta
	info     transferInfo
	sub      *bus.Subscription
	deadline time.Time
}

func (s *session) pendingChunkReady() <-chan error {
	cur := s.incomingTransfer
	if cur == nil || cur.pendingChunk == nil {
		return nil
	}
	return cur.pendingChunk.resultCh
}

func (s *session) pendingCommitReady() <-chan transferCommitResult {
	cur := s.incomingTransfer
	if cur == nil || cur.pendingCommit == nil {
		return nil
	}
	return cur.pendingCommit.resultCh
}

func (s *session) pendingTargetReady() <-chan *bus.Message {
	call := s.pendingTargetCall
	if call == nil || call.sub == nil {
		return nil
	}
	return call.sub.Channel()
}

func earlierDeadline(a time.Time, aOK bool, b time.Time, bOK bool) (time.Time, bool) {
	if !aOK {
		return b, bOK
	}
	if !bOK {
		return a, true
	}
	if b.Before(a) {
		return b, true
	}
	return a, true
}

func (s *session) nextPendingDeadline(now time.Time) (time.Time, bool) {
	var out time.Time
	var ok bool
	if cur := s.incomingTransfer; cur != nil && !cur.deadline.IsZero() {
		out, ok = earlierDeadline(out, ok, cur.deadline, true)
	}
	if call := s.pendingTargetCall; call != nil && !call.deadline.IsZero() {
		out, ok = earlierDeadline(out, ok, call.deadline, true)
	}
	for _, call := range s.inboundCalls {
		if !call.deadline.IsZero() {
			out, ok = earlierDeadline(out, ok, call.deadline, true)
		}
	}
	for _, call := range s.outboundCalls {
		if !call.deadline.IsZero() {
			out, ok = earlierDeadline(out, ok, call.deadline, true)
		}
	}
	if s.link == linkUp && !s.nextPingAt.IsZero() {
		out, ok = earlierDeadline(out, ok, s.nextPingAt, true)
	}
	if s.link == linkUp && !s.rpcReady && !s.exportReadyAt.IsZero() {
		out, ok = earlierDeadline(out, ok, s.exportReadyAt, true)
	}
	if s.link == linkUp && !s.exportDrainAt.IsZero() {
		out, ok = earlierDeadline(out, ok, s.exportDrainAt, true)
	}
	return out, ok
}

func (s *session) handlePendingDeadline(now time.Time) {
	s.checkTransferTimeout(now)
	call := s.pendingTargetCall
	if call != nil && !now.Before(call.deadline) {
		s.finishTargetCall(call, false, "stage_timeout")
	}
	s.expireInbound(now)
	s.expireOutbound(now)
	s.tickPing(now)
	s.drainBusEvents(now)
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
	if cur != nil && cur.pendingChunk != nil {
		cur.pendingChunk.data = nil
		cur.pendingChunk = nil
	}
	if cur != nil {
		cur.pendingCommit = nil
	}
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
	if cur.pendingChunk != nil {
		id := cur.meta.ID
		s.abortTransfer("chunk_write_timeout")
		abortOK := s.sendTransferAbort(id, "chunk_write_timeout")
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", "chunk_write_timeout"), otadiag.KV("ok", abortOK))
		return
	}
	if cur.pendingCommit != nil {
		id := cur.meta.ID
		s.abortTransfer("transfer_commit_timeout")
		abortOK := s.sendTransferAbort(id, "transfer_commit_timeout")
		otadiag.Event("[fabric-xfer]", "abort_tx", id, otadiag.KV("reason", "transfer_commit_timeout"), otadiag.KV("ok", abortOK))
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
	s.markRx()
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
	if s.pendingTargetCall != nil {
		reason := "busy"
		abortOK := s.sendTransferAbort(meta.ID, reason)
		otadiag.Event(
			"[fabric-xfer]", "begin_reject", meta.ID,
			otadiag.KV("reason", reason),
			otadiag.KV("pending_xfer", s.pendingTargetCall.xferID),
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

func (s *session) startPendingChunkWrite(cur *incomingTransfer, offset uint32, raw []byte) {
	ch := make(chan error, 1)
	sink := cur.sink
	data := raw
	started := time.Now()
	cur.pendingChunk = &pendingChunkWrite{
		xferID:   cur.meta.ID,
		offset:   offset,
		data:     data,
		started:  started,
		resultCh: ch,
	}
	cur.deadline = started.Add(s.cfg.PhaseTimeout)
	go func() {
		ch <- sink.WriteChunk(offset, data)
	}()
}

func (s *session) finishChunkWrite(now time.Time, err error) {
	cur := s.incomingTransfer
	if cur == nil || cur.pendingChunk == nil {
		return
	}
	pending := cur.pendingChunk
	cur.pendingChunk = nil
	if err != nil {
		reason := err.Error()
		otadiag.Event(
			"[fabric-xfer]", "sink_write_error", pending.xferID,
			otadiag.KV("reason", reason),
			otadiag.KV("dur_ms", int(time.Since(pending.started)/time.Millisecond)),
		)
		s.logKV("transfer write failed", "err", reason)
		s.abortTransfer(reason)
		abortOK := s.sendTransferAbort(pending.xferID, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", pending.xferID, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
		return
	}
	_, _ = cur.hasher.Write(pending.data)
	cur.bytesWritten += uint32(len(pending.data))
	cur.chunksSeen++
	cur.idleRetries = 0
	cur.corruptRetryOffset = cur.bytesWritten
	cur.corruptRetriesAtOffset = 0
	cur.deadline = now.Add(s.cfg.PhaseTimeout)
	otadiag.Event(
		"[fabric-xfer]", "sink_write_done", pending.xferID,
		otadiag.KV("dur_ms", int(time.Since(pending.started)/time.Millisecond)),
		otadiag.KV("next", u32s(cur.bytesWritten)),
	)
	pending.data = nil
	// Keep transfer memory bounded on TinyGo. The receiver allocates while
	// unmarshalling JSON and decoding base64 chunks; without regular collection
	// long updates can run out of heap before commit.
	gcStart := time.Now()
	otadiag.Event("[fabric-xfer]", "gc_start", pending.xferID, otadiag.KV("next", u32s(cur.bytesWritten)))
	runtime.GC()
	otadiag.Event(
		"[fabric-xfer]", "gc_done", pending.xferID,
		otadiag.KV("dur_ms", int(time.Since(gcStart)/time.Millisecond)),
		otadiag.KV("next", cur.bytesWritten),
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

func (s *session) startPendingTransferCommit(cur *incomingTransfer) {
	ch := make(chan transferCommitResult, 1)
	sink := cur.sink
	started := time.Now()
	cur.pendingCommit = &pendingTransferCommit{
		xferID:   cur.meta.ID,
		started:  started,
		resultCh: ch,
	}
	cur.deadline = started.Add(s.cfg.TargetCallTimeout)
	go func() {
		info, err := sink.Commit()
		ch <- transferCommitResult{info: info, err: err}
	}()
}

func (s *session) finishTransferCommit(now time.Time, res transferCommitResult) {
	cur := s.incomingTransfer
	if cur == nil || cur.pendingCommit == nil {
		return
	}
	pending := cur.pendingCommit
	cur.pendingCommit = nil
	if res.err != nil {
		reason := res.err.Error()
		s.logKV("transfer commit failed", "err", reason)
		s.abortTransfer(reason)
		abortOK := s.sendTransferAbort(pending.xferID, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", pending.xferID, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
		return
	}
	meta := cur.meta
	s.clearTransfer()
	otadiag.Event(
		"[fabric-xfer]", "transfer_commit_done", pending.xferID,
		otadiag.KV("dur_ms", int(time.Since(pending.started)/time.Millisecond)),
	)
	if reason := s.startTransferTargetCall(meta, pending.xferID, res.info); reason != "" {
		updater.CancelStreamedStage(pending.xferID, res.info.Generation, reason)
		abortOK := s.sendTransferAbort(pending.xferID, reason)
		otadiag.Event("[fabric-xfer]", "abort_tx", pending.xferID, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
	}
	_ = now
}

func (s *session) onTransferChunk(msg *protoXferChunk) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.XferID {
		s.logKV("xfer_chunk dropped", "id", msg.XferID)
		return
	}
	id := cur.meta.ID
	if cur.pendingChunk != nil {
		s.markRx()
		otadiag.Event(
			"[fabric-xfer]", "chunk_while_write_pending", id,
			otadiag.KV("offset", u32s(msg.Offset)),
			otadiag.KV("pending_offset", u32s(cur.pendingChunk.offset)),
			otadiag.KV("expected", u32s(cur.bytesWritten)),
		)
		return
	}
	if cur.pendingCommit != nil {
		s.markRx()
		otadiag.Event(
			"[fabric-xfer]", "chunk_while_commit_pending", id,
			otadiag.KV("offset", u32s(msg.Offset)),
			otadiag.KV("expected", u32s(cur.bytesWritten)),
		)
		return
	}
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
	s.startPendingChunkWrite(cur, msg.Offset, raw)
	otadiag.Event(
		"[fabric-xfer]", "sink_write_pending", id,
		otadiag.KV("dur_ms", int(time.Since(writeStart)/time.Millisecond)),
		otadiag.KV("offset", u32s(msg.Offset)),
	)
}

func (s *session) onTransferCommit(msg *protoXferCommit) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.XferID {
		s.logKV("xfer_commit dropped", "id", msg.XferID)
		return
	}
	id := cur.meta.ID
	if cur.pendingChunk != nil {
		s.markRx()
		otadiag.Event(
			"[fabric-xfer]", "commit_while_write_pending", id,
			otadiag.KV("expected", u32s(cur.bytesWritten)),
			otadiag.KV("pending_offset", u32s(cur.pendingChunk.offset)),
		)
		return
	}
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
	otadiag.Event("[fabric-xfer]", "transfer_commit_start", id)
	s.startPendingTransferCommit(cur)
}

// startTransferTargetCall invokes the local updater/main staging RPC without
// blocking the Fabric session reactor. The reply channel is selected directly
// by session.run, so completion wakes the reactor without waiting for a
// periodic tick.
func (s *session) startTransferTargetCall(meta transferMeta, xferID string, info transferInfo) string {
	if meta.Target != transferTargetUpdaterMain {
		return "unsupported_target"
	}
	if s.pendingTargetCall != nil {
		return "busy"
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
	}
	msg := s.conn.NewMessage(updater.TopicStageRPC, payload, false)
	s.pendingTargetCall = &pendingTargetCall{
		xferID:   xferID,
		meta:     meta,
		info:     info,
		sub:      s.conn.Request(msg),
		deadline: time.Now().Add(s.cfg.TargetCallTimeout),
	}
	otadiag.Event("[fabric-xfer]", "target_call_start", xferID,
		otadiag.KV("timeout_ms", int(s.cfg.TargetCallTimeout/time.Millisecond)),
	)
	return ""
}

func (s *session) finishTargetCall(call *pendingTargetCall, ok bool, reason string) {
	if call == nil {
		return
	}
	if call.sub != nil {
		s.conn.Unsubscribe(call.sub)
		call.sub = nil
	}
	s.pendingTargetCall = nil
	if ok {
		s.recordCompletedTransfer(call.meta)
		doneOK := s.sendTransferDone(call.xferID)
		otadiag.Event("[fabric-xfer]", "done_tx", call.xferID, otadiag.KV("ok", doneOK))
		otadiag.StopUpdateWindow("transfer_done")
		return
	}
	if reason == "" {
		reason = "stage_rejected"
	}
	updater.CancelStreamedStage(call.xferID, call.info.Generation, reason)
	abortOK := s.sendTransferAbort(call.xferID, reason)
	otadiag.Event("[fabric-xfer]", "abort_tx", call.xferID, otadiag.KV("reason", reason), otadiag.KV("ok", abortOK))
}

func (s *session) finishTargetReply(rep *bus.Message, ok bool) {
	call := s.pendingTargetCall
	if call == nil {
		return
	}
	if !ok || rep == nil {
		s.finishTargetCall(call, false, "stage_no_reply")
		return
	}
	okReply, reason := decodeStageReply(rep.Payload)
	s.finishTargetCall(call, okReply, reason)
}

func (s *session) cancelTargetCall(reason string) {
	call := s.pendingTargetCall
	if call == nil {
		return
	}
	if reason == "" {
		reason = reasonLinkDown
	}
	if call.sub != nil {
		s.conn.Unsubscribe(call.sub)
		call.sub = nil
	}
	s.pendingTargetCall = nil
	updater.CancelStreamedStage(call.xferID, call.info.Generation, reason)
	otadiag.Event("[fabric-xfer]", "target_call_cancel", call.xferID, otadiag.KV("reason", reason))
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

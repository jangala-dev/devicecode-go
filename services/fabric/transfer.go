package fabric

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"devicecode-go/services/updater"
	"devicecode-go/x/strconvx"
	"devicecode-go/x/xxhash"
)

const transferTargetUpdaterMain = "updater/main"
const transferIdleRetryLimit = 12
const transferOffsetRetryLimit = 12
const transferCorruptRetryLimit = transferOffsetRetryLimit // compatibility for existing transfer tests
const completedTransferCacheLimit = 4
const transferProgressLogStep uint32 = 128 * 1024
const transferRetryMinInterval = 200 * time.Millisecond
const transferRetryJitterWindow = 750 * time.Millisecond
const transferMaxNoProgress = 90 * time.Second

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
	cancel       func(reason string)
}

func (i transferInfo) cancelStage(reason string) {
	if i.cancel != nil {
		i.cancel(reason)
	}
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
	meta              transferMeta
	sink              transferSink
	bytesWritten      uint32
	chunksSeen        uint32
	hasher            *xxhash.Hasher
	idleRetries       uint8
	retryOffset       uint32
	retriesAtOffset   uint8
	lastNeedAt        time.Time
	lastRetryReason   string
	lastProgressAt    time.Time
	nextProgressLogAt uint32
	chunkRejectLogs   uint8
	// deadline is the idle-chunk watchdog: bumped on every accepted chunk
	// and on initial xfer_begin. checkTransferTimeout fires if now > deadline.
	// Mirrors transfer_mgr.lua: `active.deadline = runtime.now() + phase_timeout`.
	// The receiver is deliberately synchronous: an xfer_need is not sent until
	// the current chunk has been decoded, verified and written to the stage sink.
	deadline time.Time
}

type completedTransfer struct {
	meta transferMeta
}

type pendingTargetCall struct {
	xferID   string
	meta     transferMeta
	info     transferInfo
	deadline time.Time
	cancel   context.CancelFunc
}

type targetCallResult struct {
	call   *pendingTargetCall
	ok     bool
	reason string
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

func canonicalXXHash32Hex(s string) (string, bool) {
	return s, validXXHash32Hex(s)
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

func nextTransferProgressAfter(v uint32) uint32 {
	next := ((v / transferProgressLogStep) + 1) * transferProgressLogStep
	if next <= v {
		return v + transferProgressLogStep
	}
	return next
}

func (cur *incomingTransfer) shouldLogProgress() bool {
	if cur == nil {
		return false
	}
	if cur.bytesWritten == 0 {
		return false
	}
	if cur.nextProgressLogAt == 0 {
		cur.nextProgressLogAt = transferProgressLogStep
	}
	if cur.bytesWritten < cur.nextProgressLogAt && cur.bytesWritten < cur.meta.Size {
		return false
	}
	for cur.nextProgressLogAt <= cur.bytesWritten && cur.nextProgressLogAt < cur.meta.Size {
		cur.nextProgressLogAt = nextTransferProgressAfter(cur.nextProgressLogAt)
	}
	return true
}

func (s *session) decodeChunkData(encoded string) ([]byte, string) {
	return s.decodeChunkDataBytes([]byte(encoded))
}

func (s *session) decodeChunkDataBytes(encoded []byte) ([]byte, string) {
	s.buffers = ensureFabricBuffers(s.buffers)
	maxAccepted := int(s.cfg.MaxAcceptedChunkSize)
	if maxAccepted <= 0 || maxAccepted > len(s.buffers.ChunkRaw) {
		maxAccepted = len(s.buffers.ChunkRaw)
	}
	if len(encoded) > len(s.buffers.ChunkB64) {
		return nil, "chunk_too_large"
	}
	decodedLen := base64.RawURLEncoding.DecodedLen(len(encoded))
	if decodedLen > maxAccepted {
		return nil, "chunk_too_large"
	}
	raw := s.buffers.ChunkRaw[:maxAccepted]
	n, err := base64.RawURLEncoding.Decode(raw, encoded)
	if err != nil {
		return nil, "invalid_chunk_encoding"
	}
	encLen := base64.RawURLEncoding.EncodedLen(n)
	if encLen != len(encoded) || encLen > len(s.buffers.ChunkB64) {
		return nil, "invalid_chunk_encoding"
	}
	base64.RawURLEncoding.Encode(s.buffers.ChunkB64[:encLen], raw[:n])
	for i := 0; i < encLen; i++ {
		if s.buffers.ChunkB64[i] != encoded[i] {
			return nil, "invalid_chunk_encoding"
		}
	}
	return raw[:n], ""
}

func (s *session) sendTransferReady(id string) bool {
	ok := s.sendControl(marshalXferReady(id))
	return ok
}

func (s *session) sendTransferNeed(id string, next uint32) bool {
	ok := s.sendControl(marshalXferNeed(id, next))
	return ok
}

func (s *session) sendTransferRetryNeed(id string, next uint32, reason string) bool {
	ok := s.sendControl(marshalXferNeedWithReason(id, next, true, reason))
	return ok
}

func (s *session) sendTransferDone(id string) bool {
	ok := s.sendControl(marshalXferDone(id))
	return ok
}

func (s *session) sendTransferAbort(id, reason string) bool {
	ok := s.sendControl(marshalXferAbort(id, reason))
	return ok
}

func (s *session) clearTransfer() *incomingTransfer {
	cur := s.incomingTransfer
	s.incomingTransfer = nil
	return cur
}

func (s *session) abortTransfer(reason string) {
	s.counters.TransferAborts++
	cur := s.clearTransfer()
	if cur == nil {
		return
	}
	if cur.sink != nil {
		_ = cur.sink.Abort(reason)
	}
}

func (s *session) clearTransferAfterSinkFailure(reason string) {
	cur := s.clearTransfer()
	if cur == nil {
		return
	}
	if cur.sink != nil {
		_ = cur.sink.Abort(reason)
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
	if s.abortIfTransferNoProgress(cur, now, "timeout") {
		return
	}
	if cur.idleRetries >= transferIdleRetryLimit {
		id := cur.meta.ID
		s.abortTransfer("timeout")
		_ = s.sendTransferAbort(id, "timeout")
		logFabricXferAbort(id, "timeout", cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	cur.idleRetries++
	s.counters.TransferIdleRetries++
	_ = s.requestTransferRetry("idle", true)
}

func transferRetryJitter(id string, next uint32, retries uint8) time.Duration {
	var h uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	h ^= next
	h *= 16777619
	h ^= uint32(retries)
	return time.Duration(h%uint32(transferRetryJitterWindow/time.Millisecond+1)) * time.Millisecond
}

func (s *session) abortIfTransferNoProgress(cur *incomingTransfer, now time.Time, reason string) bool {
	if cur == nil || cur.lastProgressAt.IsZero() {
		return false
	}
	if now.Sub(cur.lastProgressAt) <= transferMaxNoProgress {
		return false
	}
	id := cur.meta.ID
	s.abortTransfer(reason)
	_ = s.sendTransferAbort(id, reason)
	logFabricXferAbort(id, reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
	return true
}

func (s *session) requestTransferRetry(reason string, force bool) bool {
	cur := s.incomingTransfer
	if cur == nil {
		return false
	}
	now := time.Now()
	s.markRx()
	if s.abortIfTransferNoProgress(cur, now, reason) {
		return false
	}
	if cur.retryOffset != cur.bytesWritten {
		cur.retryOffset = cur.bytesWritten
		cur.retriesAtOffset = 0
		cur.lastRetryReason = ""
		cur.lastNeedAt = time.Time{}
	}
	if cur.retriesAtOffset >= transferOffsetRetryLimit {
		id := cur.meta.ID
		s.abortTransfer(reason)
		_ = s.sendTransferAbort(id, reason)
		logFabricXferAbort(id, reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return false
	}
	if !force && !cur.lastNeedAt.IsZero() && now.Sub(cur.lastNeedAt) < transferRetryMinInterval && cur.lastRetryReason == reason {
		return true
	}
	s.counters.TransferOffsetRetries++
	if reason == "bad_json" || reason == "line_too_long" || reason == "bad_frame" || reason == "bad_message" {
		s.counters.TransferBadFrameRetries++
	}
	cur.retriesAtOffset++
	cur.lastNeedAt = now
	cur.lastRetryReason = reason
	cur.deadline = now.Add(s.cfg.PhaseTimeout + transferRetryJitter(cur.meta.ID, cur.bytesWritten, cur.retriesAtOffset))
	return s.sendTransferRetryNeed(cur.meta.ID, cur.bytesWritten, reason)
}

func (s *session) retryCorruptTransferFrame(reason string) bool {
	// A parsed xfer_chunk for the active transfer is a direct response to our
	// current xfer_need. If it is malformed, has bad encoding, or fails digest
	// verification, promptly re-advertise the same offset. Generic bad JSON or
	// unknown-frame loss is still rate-limited at the caller.
	return s.requestTransferRetry(reason, true)
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
	meta, errStr := validateTransferBegin(msg)
	if errStr != "" {
		if msg.XferID != "" {
			_ = s.sendTransferAbort(msg.XferID, "bad_message: "+errStr)
			logFabricXferAbort(msg.XferID, "bad_message: "+errStr, 0, 0, 0, 0)
		}
		s.logKV("xfer_begin dropped", "err", errStr)
		return
	}
	s.markRx()
	now := time.Now()
	if s.incomingTransfer != nil {
		cur := s.incomingTransfer
		if sameTransferTuple(cur.meta, meta) {
			s.logKV("xfer_begin duplicate", "id", meta.ID)
			readyOK := s.sendTransferReady(meta.ID)
			if readyOK {
				_ = s.sendTransferNeed(meta.ID, cur.bytesWritten)
			}
			cur.deadline = now.Add(s.cfg.PhaseTimeout)
			return
		}
		reason := "busy"
		if cur.meta.ID == meta.ID {
			reason = "conflicting_transfer"
		}
		_ = s.sendTransferAbort(meta.ID, reason)
		logFabricXferAbort(meta.ID, reason, 0, meta.Size, 0, 0)
		return
	}
	if s.pendingTargetCall != nil {
		reason := "busy"
		_ = s.sendTransferAbort(meta.ID, reason)
		logFabricXferAbort(meta.ID, reason, 0, meta.Size, 0, 0)
		return
	}
	if done, ok := s.completedTransferFor(meta.ID); ok {
		if sameTransferTuple(done, meta) {
			_ = s.sendTransferDone(meta.ID)
			return
		}
		_ = s.sendTransferAbort(meta.ID, "conflicting_transfer")
		logFabricXferAbort(meta.ID, "conflicting_transfer", 0, meta.Size, 0, 0)
		return
	}
	beginFn := s.beginTransfer
	if beginFn == nil {
		beginFn = func(meta transferMeta) (transferSink, error) {
			return beginUpdaterTransfer(s.stageController, meta)
		}
	}
	sink, err := beginFn(meta)
	if err != nil {
		_ = s.sendTransferAbort(meta.ID, err.Error())
		logFabricXferAbort(meta.ID, err.Error(), 0, meta.Size, 0, 0)
		return
	}
	s.counters.TransferBegins++
	logFabricXferBegin(meta.ID, meta.Target, meta.Size)
	s.incomingTransfer = &incomingTransfer{
		meta:              meta,
		sink:              sink,
		hasher:            xxhash.New(0),
		nextProgressLogAt: transferProgressLogStep,
		deadline:          now.Add(s.cfg.PhaseTimeout),
		lastProgressAt:    now,
		retryOffset:       0,
	}
	readyOK := s.sendTransferReady(meta.ID)
	if readyOK {
		_ = s.sendTransferNeed(meta.ID, 0)
	} else {
	}
}

func (s *session) acceptTransferChunk(cur *incomingTransfer, offset uint32, raw []byte) bool {
	start := time.Now()
	if cur.sink == nil {
		s.abortAcceptedTransferChunk(cur, "transfer_sink_missing", start)
		return false
	}
	// Hash before handing the slice to the sink. The sink may copy, stream,
	// or otherwise take ownership of the bytes during WriteChunk; Fabric must
	// not rely on the mutable decode scratch buffer after WriteChunk returns.
	_, _ = cur.hasher.Write(raw)
	if err := cur.sink.WriteChunk(offset, raw); err != nil {
		s.abortAcceptedTransferChunk(cur, err.Error(), start)
		return false
	}
	writeMS := int(time.Since(start) / time.Millisecond)
	if time.Since(start) > s.cfg.PhaseTimeout {
		reason := "chunk_write_timeout"
		_ = cur.sink.Abort(reason)
		s.abortAcceptedTransferChunk(cur, reason, start)
		return false
	}
	cur.bytesWritten += uint32(len(raw))
	cur.chunksSeen++
	s.counters.TransferChunks++
	s.counters.TransferBytes += uint64(len(raw))
	cur.idleRetries = 0
	cur.retryOffset = cur.bytesWritten
	cur.retriesAtOffset = 0
	cur.lastRetryReason = ""
	cur.lastNeedAt = time.Time{}
	now := time.Now()
	cur.lastProgressAt = now
	cur.deadline = now.Add(s.cfg.PhaseTimeout)
	if cur.shouldLogProgress() {
		logFabricXferProgress(cur.meta.ID, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, writeMS)
	}
	_ = s.sendTransferNeed(cur.meta.ID, cur.bytesWritten)
	return true
}

func (s *session) abortAcceptedTransferChunk(cur *incomingTransfer, reason string, started time.Time) {
	if cur == nil {
		return
	}
	id := cur.meta.ID
	_ = started
	s.counters.TransferWriteErrors++
	s.logKV("transfer write failed", "err", reason)
	s.clearTransferAfterSinkFailure(reason)
	_ = s.sendTransferAbort(id, reason)
	logFabricXferAbort(id, reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
}

func (s *session) commitTransfer(cur *incomingTransfer) {
	start := time.Now()
	if cur.sink == nil {
		s.abortTransfer("transfer_sink_missing")
		_ = s.sendTransferAbort(cur.meta.ID, "transfer_sink_missing")
		logFabricXferAbort(cur.meta.ID, "transfer_sink_missing", cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	info, err := cur.sink.Commit()
	if err != nil {
		reason := err.Error()
		s.counters.TransferCommitErrors++
		s.logKV("transfer commit failed", "err", reason)
		s.clearTransferAfterSinkFailure(reason)
		_ = s.sendTransferAbort(cur.meta.ID, reason)
		logFabricXferAbort(cur.meta.ID, reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	if time.Since(start) > s.cfg.TargetCallTimeout {
		reason := "transfer_commit_timeout"
		info.cancelStage(reason)
		s.clearTransferAfterSinkFailure(reason)
		_ = s.sendTransferAbort(cur.meta.ID, reason)
		logFabricXferAbort(cur.meta.ID, reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	meta := cur.meta
	id := meta.ID
	s.clearTransfer()
	if reason := s.startTransferTargetCall(meta, id, info); reason != "" {
		info.cancelStage(reason)
		_ = s.sendTransferAbort(id, reason)
		logFabricXferAbort(id, reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
	}
}

func (s *session) onTransferChunk(msg *protoXferChunk) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.XferID {
		s.logKV("xfer_chunk dropped", "id", msg.XferID)
		return
	}
	id := cur.meta.ID
	if msg.Offset < cur.bytesWritten {
		s.markRx()
		_ = s.sendTransferNeed(id, cur.bytesWritten)
		return
	}
	if msg.Offset > cur.bytesWritten {
		s.markRx()
		_ = s.sendTransferNeed(id, cur.bytesWritten)
		return
	}
	raw, errStr := s.decodeChunkDataBytes(msg.dataBytes())
	if errStr != "" {
		s.counters.TransferDecodeErrors++
		s.counters.TransferChunkRejects++
		if cur.chunkRejectLogs == 0 {
			logFabricXferReject(id, errStr, msg.Offset, cur.bytesWritten, 0, msg.dataLen(), msg.LineLen, "-", "-")
			cur.chunkRejectLogs++
		}
		s.logKV("xfer_chunk decode retry", "err", errStr)
		s.retryCorruptTransferFrame(errStr)
		return
	}
	if len(raw) == 0 {
		s.abortTransfer("empty_chunk")
		_ = s.sendTransferAbort(id, "empty_chunk")
		logFabricXferAbort(id, "empty_chunk", cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	if cur.bytesWritten+uint32(len(raw)) > cur.meta.Size {
		reason := "size_too_large"
		s.abortTransfer(reason)
		_ = s.sendTransferAbort(id, reason)
		logFabricXferAbort(id, reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	// Per-chunk integrity is required by the current MCU contract.
	// JSON parsing alone misses single-byte UART corruption inside the
	// base64url data string: the bytes still decode, just to the wrong
	// values. On mismatch we ask the sender to resume at the current
	// byte offset instead of clearing the transfer.
	want, ok := canonicalXXHash32Hex(msg.ChunkDigest)
	if !ok {
		s.counters.TransferDigestErrors++
		s.counters.TransferChunkRejects++
		if cur.chunkRejectLogs == 0 {
			logFabricXferReject(id, "bad_message", msg.Offset, cur.bytesWritten, len(raw), msg.dataLen(), msg.LineLen, msg.ChunkDigest, "-")
			cur.chunkRejectLogs++
		}
		s.retryCorruptTransferFrame("bad_message")
		return
	}
	gotDigest := xxhash.Sum32(raw, 0)
	if !xxhashHexEqual(gotDigest, want) {
		s.counters.TransferDigestErrors++
		s.counters.TransferChunkRejects++
		if cur.chunkRejectLogs == 0 {
			logFabricXferReject(id, "chunk_digest_mismatch", msg.Offset, cur.bytesWritten, len(raw), msg.dataLen(), msg.LineLen, want, xxhashHex(gotDigest))
			cur.chunkRejectLogs++
		}
		s.retryCorruptTransferFrame("chunk_digest_mismatch")
		return
	}
	s.markRx()
	s.acceptTransferChunk(cur, msg.Offset, raw)
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
		_ = s.sendTransferAbort(id, reason)
		logFabricXferAbort(id, reason, cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	if msg.DigestAlg != digestAlg {
		s.abortTransfer("unsupported_digest_alg")
		_ = s.sendTransferAbort(id, "unsupported_digest_alg")
		logFabricXferAbort(id, "unsupported_digest_alg", cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	commitDigest, ok := canonicalXXHash32Hex(msg.Digest)
	if !ok {
		s.abortTransfer("invalid_digest")
		_ = s.sendTransferAbort(id, "invalid_digest")
		logFabricXferAbort(id, "invalid_digest", cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	streamedDigest := cur.hasher.Sum32()
	if commitDigest != cur.meta.Digest || !xxhashHexEqual(streamedDigest, cur.meta.Digest) {
		s.abortTransfer("digest_mismatch")
		_ = s.sendTransferAbort(id, "digest_mismatch")
		logFabricXferAbort(id, "digest_mismatch", cur.bytesWritten, cur.meta.Size, cur.chunksSeen, cur.idleRetries)
		return
	}
	s.markRx()
	s.commitTransfer(cur)
}

// startTransferTargetCall queues the local updater/main staging RPC in a
// worker goroutine. The Fabric session reactor keeps servicing UART while the
// updater performs staged-image validation and flash bookkeeping.
func (s *session) startTransferTargetCall(meta transferMeta, xferID string, info transferInfo) string {
	if meta.Target != transferTargetUpdaterMain {
		return "unsupported_target"
	}
	if s.pendingTargetCall != nil {
		return "busy"
	}
	if s.targetCallResults == nil {
		s.targetCallResults = make(chan targetCallResult, 1)
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
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, s.cfg.TargetCallTimeout)
	call := &pendingTargetCall{xferID: xferID, meta: meta, info: info, deadline: time.Now().Add(s.cfg.TargetCallTimeout), cancel: cancel}
	s.pendingTargetCall = call
	go s.runTargetCall(ctx, call, payload)
	return ""
}

func (s *session) runTargetCall(ctx context.Context, call *pendingTargetCall, payload updater.StagePayload) {
	reply, err := s.conn.Call(ctx, updater.TopicStageRPC, payload)
	result := targetCallResult{call: call}
	if err != nil {
		result.reason = err.Error()
		if result.reason == "bus: no_route" {
			result.reason = reasonNoRoute
		}
	} else {
		result.ok, result.reason = decodeStageReply(reply)
	}
	select {
	case s.targetCallResults <- result:
	case <-ctx.Done():
	}
}

func (s *session) handleTargetCallResult(result targetCallResult) {
	call := result.call
	if call == nil || s.pendingTargetCall != call {
		return
	}
	logFabricXferStageReply(call.xferID, result.ok, result.reason, call.info.BytesWritten, call.info.Generation)
	s.finishTargetCall(call, result.ok, result.reason)
}

func (s *session) finishTargetCall(call *pendingTargetCall, ok bool, reason string) {
	if call == nil {
		return
	}
	if call.cancel != nil {
		call.cancel()
		call.cancel = nil
	}
	s.pendingTargetCall = nil
	if ok {
		s.counters.TransferCompletions++
		s.recordCompletedTransfer(call.meta)
		_ = s.sendTransferDone(call.xferID)
		logFabricXferDone(call.xferID, call.info.BytesWritten, 0)
		return
	}
	if reason == "" {
		reason = "stage_rejected"
	}
	call.info.cancelStage(reason)
	_ = s.sendTransferAbort(call.xferID, reason)
	logFabricXferAbort(call.xferID, reason, call.info.BytesWritten, call.meta.Size, 0, 0)
}

func (s *session) cancelTargetCall(reason string) {
	call := s.pendingTargetCall
	if call == nil {
		return
	}
	if reason == "" {
		reason = reasonLinkDown
	}
	s.pendingTargetCall = nil
	if call.cancel != nil {
		call.cancel()
		call.cancel = nil
	}
	call.info.cancelStage(reason)
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
}

// xxhashHex formats a uint32 xxHash32 digest as 8 lower-case hex characters,
// matching the wire format used by the Lua reference's M.digest_hex.

func xxhashHexEqual(v uint32, s string) bool {
	if len(s) != 8 {
		return false
	}
	const digits = "0123456789abcdef"
	for i := 7; i >= 0; i-- {
		if s[i] != digits[v&0xf] {
			return false
		}
		v >>= 4
	}
	return true
}

func xxhashHex(v uint32) string {
	const digits = "0123456789abcdef"
	var buf [8]byte
	for i := 7; i >= 0; i-- {
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

func byteHexPrefix(b []byte, n int) string {
	if n < 0 {
		n = 0
	}
	if n > len(b) {
		n = len(b)
	}
	const digits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := b[i]
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0xf]
	}
	return string(out)
}

func stringPrefix(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if n > len(s) {
		n = len(s)
	}
	return s[:n]
}

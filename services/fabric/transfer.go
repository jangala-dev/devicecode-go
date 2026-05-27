package fabric

import (
	"encoding/base64"
	"encoding/json"
	"runtime"
	"strings"
	"time"

	"devicecode-go/services/updater"
	"devicecode-go/x/strconvx"
	"devicecode-go/x/xxhash"
)

const transferTargetUpdaterMain = "updater/main"
const transferIdleRetryLimit = 3

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
	meta         transferMeta
	sink         transferSink
	bytesWritten uint32
	chunksSeen   uint32
	hasher       *xxhash.Hasher
	idleRetries  uint8
	// deadline is the idle-chunk watchdog: bumped on every accepted chunk
	// and on initial xfer_begin. checkTransferTimeout fires if now > deadline.
	// Mirrors transfer_mgr.lua: `active.deadline = runtime.now() + phase_timeout`.
	deadline time.Time
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
	s.sendTransferAbort(id, "timeout")
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
	meta, errStr := validateTransferBegin(msg)
	if errStr != "" {
		if msg.XferID != "" {
			s.sendTransferAbort(msg.XferID, "bad_message: "+errStr)
		}
		s.logKV("xfer_begin dropped", "err", errStr)
		return
	}
	if s.incomingTransfer != nil {
		cur := s.incomingTransfer
		if cur.meta.ID == meta.ID &&
			cur.meta.Size == meta.Size &&
			cur.meta.Target == meta.Target &&
			cur.meta.DigestAlg == meta.DigestAlg &&
			cur.meta.Digest == meta.Digest {
			s.logKV("xfer_begin duplicate", "id", meta.ID)
			if s.sendTransferReady(meta.ID) {
				s.sendTransferNeed(meta.ID, cur.bytesWritten)
			}
			cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
			return
		}
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
	if s.sendTransferReady(meta.ID) {
		s.sendTransferNeed(meta.ID, 0)
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
		s.sendTransferNeed(id, cur.bytesWritten)
		cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
		return
	}
	if msg.Offset > cur.bytesWritten {
		s.sendTransferNeed(id, cur.bytesWritten)
		return
	}
	raw, errStr := decodeChunkData(msg.Data)
	if errStr != "" {
		s.logKV("xfer_chunk decode retry", "err", errStr)
		s.sendTransferNeed(id, cur.bytesWritten)
		cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
		return
	}
	if len(raw) == 0 {
		s.abortTransfer("empty_chunk")
		s.sendTransferAbort(id, "empty_chunk")
		return
	}
	if cur.bytesWritten+uint32(len(raw)) > cur.meta.Size {
		reason := "size_too_large"
		s.abortTransfer(reason)
		s.sendTransferAbort(id, reason)
		return
	}
	// Per-chunk integrity is required by the current MCU contract.
	// JSON parsing alone misses single-byte UART corruption inside the
	// base64url data string: the bytes still decode, just to the wrong
	// values. On mismatch we ask the sender to resume at the current
	// byte offset instead of clearing the transfer.
	want, ok := canonicalXXHash32Hex(msg.ChunkDigest)
	if !ok {
		reason := "invalid_chunk_digest"
		if msg.ChunkDigest == "" {
			reason = "missing_chunk_digest"
		}
		println(
			"[fabric-xfer]", "abort_tx",
			"id", id,
			"reason", reason,
			"offset", u32s(msg.Offset),
			"digest_len", strconvx.Itoa(len(msg.ChunkDigest)),
			"digest", msg.ChunkDigest,
			"data_len", strconvx.Itoa(len(msg.Data)),
		)
		s.abortTransfer(reason)
		s.sendTransferAbort(id, reason)
		return
	}
	got := xxhashHex(xxhash.Sum32(raw, 0))
	if got != want {
		s.sendTransferNeed(cur.meta.ID, cur.bytesWritten)
		// Recovery counts as progress — bump the deadline so a burst
		// of digest-mismatched chunks doesn't trip the idle watchdog
		// mid-recovery.
		cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
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
	cur.idleRetries = 0
	cur.deadline = time.Now().Add(s.cfg.PhaseTimeout)
	raw = nil
	// Keep transfer memory bounded on TinyGo. The receiver allocates while
	// unmarshalling JSON and decoding base64 chunks; without regular collection
	// long updates can run out of heap before commit.
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
		reason := "short_transfer"
		s.abortTransfer(reason)
		s.sendTransferAbort(id, reason)
		return
	}
	if msg.DigestAlg != digestAlg {
		s.abortTransfer("unsupported_digest_alg")
		s.sendTransferAbort(id, "unsupported_digest_alg")
		return
	}
	commitDigest, ok := canonicalXXHash32Hex(msg.Digest)
	if !ok {
		s.abortTransfer("invalid_digest")
		s.sendTransferAbort(id, "invalid_digest")
		return
	}
	streamedHex := xxhashHex(cur.hasher.Sum32())
	if commitDigest != cur.meta.Digest || streamedHex != cur.meta.Digest {
		s.abortTransfer("digest_mismatch")
		s.sendTransferAbort(id, "digest_mismatch")
		return
	}
	_, err := cur.sink.Commit()
	if err != nil {
		s.logKV("transfer commit failed", "err", err.Error())
		reason := err.Error()
		s.abortTransfer(reason)
		s.sendTransferAbort(id, reason)
		return
	}
	sink := cur.sink
	meta := cur.meta
	s.extendTransferQuiet("xfer_commit_target", transferCompleteQuiet)
	s.clearTransfer()

	bytesPayload := sink.Bytes()
	ok, reason := s.invokeTransferTarget(meta, id, bytesPayload)
	if !ok {
		s.extendTransferQuiet("xfer_target_rejected", transferCompleteQuiet)
		s.sendTransferAbort(id, reason)
		return
	}
	s.extendTransferQuiet("xfer_done", transferCompleteQuiet)
	s.sendTransferDone(id)
}

const targetCallTimeout = 5 * time.Second

// invokeTransferTarget calls the local updater staging RPC named by
// xfer_begin.target. The wire no longer carries raw/member receiver topics;
// target="updater/main" maps to an internal bus RPC owned by the updater
// service. The reply gates whether fabric sends xfer_done or xfer_abort.
func (s *session) invokeTransferTarget(meta transferMeta, xferID string, artefact []byte) (bool, string) {
	if meta.Target != transferTargetUpdaterMain {
		return false, "unsupported_target"
	}
	payload := updater.StagePayload{
		LinkID:    s.linkID,
		XferID:    xferID,
		Target:    meta.Target,
		Size:      meta.Size,
		DigestAlg: meta.DigestAlg,
		Digest:    meta.Digest,
		Meta:      meta.Meta,
		Artefact:  artefact,
	}
	msg := s.conn.NewMessage(updater.TopicStageRPC, payload, false)
	replySub := s.conn.Request(msg)
	defer s.conn.Unsubscribe(replySub)

	select {
	case rep, ok := <-replySub.Channel():
		if !ok || rep == nil {
			return false, "stage_no_reply"
		}
		ok, reason := decodeStageReply(rep.Payload)
		if !ok {
			return false, reason
		}
		return true, ""
	case <-time.After(targetCallTimeout):
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

package fabric

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"runtime"
	"strings"
	"time"

	"devicecode-go/x/strconvx"
)

const postTransferDoneSettle = 250 * time.Millisecond
const transferProgressLogEvery = 32

type transferMeta struct {
	ID       string
	Kind     string
	Name     string
	Format   string
	Enc      string
	Size     uint32
	ChunkRaw uint32
	Chunks   uint32
	SHA256   string
	Meta     json.RawMessage
}

type transferInfo struct {
	BytesWritten uint32 `json:"bytes_written,omitempty"`
	SlotXIPAddr  uint32 `json:"slot_xip_addr,omitempty"`
}

func (i transferInfo) isZero() bool {
	return i.BytesWritten == 0 && i.SlotXIPAddr == 0
}

type transferFactory interface {
	Begin(meta transferMeta) (transferSink, error)
}

type transferSink interface {
	WriteChunk(seq, off uint32, data []byte) error
	Commit() (transferInfo, error)
	Apply() error
	Abort(reason string) error
}

type incomingTransfer struct {
	meta         transferMeta
	sink         transferSink
	expectedNext uint32
	bytesWritten uint32
	chunksSeen   uint32
	hasher       hash.Hash
}

func lowerHex(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func crc32Hex(data []byte) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE(data))
}

func sha256Hex(h hash.Hash) string {
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)
}

func readyNext(v uint32) *uint32 {
	return &v
}

func u32s(v uint32) string {
	return strconvx.Itoa(int(v))
}

func textPreview(s string) string {
	return tracePreview([]byte(s))
}

func textTailPreview(s string) string {
	return traceTailPreview([]byte(s))
}

func infoPayload(info transferInfo) json.RawMessage {
	if info.isZero() {
		return nil
	}
	b, err := json.Marshal(info)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

func (s *session) sendTransferReady(id string, ok bool, next *uint32, errStr string) bool {
	return s.sendFrame(marshal(protoXferReady{
		T:    msgXferReady,
		ID:   id,
		OK:   ok,
		Next: next,
		Err:  errStr,
	}))
}

func (s *session) sendTransferNeed(id string, next uint32, errStr string) bool {
	return s.sendFrame(marshal(protoXferNeed{
		T:    msgXferNeed,
		ID:   id,
		Next: next,
		Err:  errStr,
	}))
}

func (s *session) sendTransferDone(id string, ok bool, info transferInfo, errStr string) bool {
	return s.sendFrame(marshal(protoXferDone{
		T:    msgXferDone,
		ID:   id,
		OK:   ok,
		Info: infoPayload(info),
		Err:  errStr,
	}))
}

func (s *session) sendTransferAbort(id, reason string) bool {
	return s.sendFrame(marshal(protoXferAbort{
		T:      msgXferAbort,
		ID:     id,
		Reason: reason,
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

func validateTransferBegin(msg *protoMsg) (transferMeta, string) {
	if msg.ID == "" {
		return transferMeta{}, "xfer_begin.id"
	}
	if msg.Kind == "" {
		return transferMeta{}, "xfer_begin.kind"
	}
	if msg.Name == "" {
		return transferMeta{}, "xfer_begin.name"
	}
	if msg.Format == "" {
		return transferMeta{}, "xfer_begin.format"
	}
	if msg.Enc == "" {
		return transferMeta{}, "xfer_begin.enc"
	}
	if msg.Size == 0 {
		return transferMeta{}, "xfer_begin.size"
	}
	if msg.ChunkRaw == 0 {
		return transferMeta{}, "xfer_begin.chunk_raw"
	}
	if msg.Chunks == 0 {
		return transferMeta{}, "xfer_begin.chunks"
	}
	if msg.SHA256 == "" {
		return transferMeta{}, "xfer_begin.sha256"
	}
	return transferMeta{
		ID:       msg.ID,
		Kind:     msg.Kind,
		Name:     msg.Name,
		Format:   msg.Format,
		Enc:      msg.Enc,
		Size:     msg.Size,
		ChunkRaw: msg.ChunkRaw,
		Chunks:   msg.Chunks,
		SHA256:   lowerHex(msg.SHA256),
		Meta:     append(json.RawMessage(nil), msg.Meta...),
	}, ""
}

func (s *session) onTransferBegin(msg *protoMsg) {
	meta, errStr := validateTransferBegin(msg)
	if errStr != "" {
		if msg.ID != "" {
			s.sendTransferReady(msg.ID, false, nil, "bad_message: "+errStr)
		}
		s.logKV("xfer_begin dropped", "err", errStr)
		return
	}
	if s.incomingTransfer != nil {
		s.sendTransferReady(meta.ID, false, nil, "busy")
		return
	}
	if meta.Enc != "b64url" {
		s.sendTransferReady(meta.ID, false, nil, "unsupported_encoding")
		return
	}
	if s.transferFactory == nil {
		s.transferFactory = newTransferFactory()
	}
	sink, err := s.transferFactory.Begin(meta)
	if err != nil {
		s.sendTransferReady(meta.ID, false, nil, err.Error())
		return
	}
	s.incomingTransfer = &incomingTransfer{
		meta:   meta,
		sink:   sink,
		hasher: sha256.New(),
	}
	println(
		"[fabric]", "sid", s.localSID,
		"xfer_begin accepted",
		"id", meta.ID,
		"kind", meta.Kind,
		"size", u32s(meta.Size),
		"chunks", u32s(meta.Chunks),
		"chunk_raw", u32s(meta.ChunkRaw),
	)
	s.sendTransferReady(meta.ID, true, readyNext(0), "")
}

func (s *session) onTransferChunk(msg *protoMsg) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.ID {
		s.logKV("xfer_chunk dropped", "id", msg.ID)
		return
	}
	if msg.Seq != cur.expectedNext {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_need sent",
			"id", cur.meta.ID,
			"next", u32s(cur.expectedNext),
			"err", "unexpected_seq",
			"seq", u32s(msg.Seq),
		)
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "unexpected_seq")
		return
	}
	if msg.Off != cur.bytesWritten {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_need sent",
			"id", cur.meta.ID,
			"next", u32s(cur.expectedNext),
			"err", "unexpected_offset",
			"off", u32s(msg.Off),
			"want_off", u32s(cur.bytesWritten),
		)
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "unexpected_offset")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(msg.Data)
	if err != nil {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_need sent",
			"id", cur.meta.ID,
			"next", u32s(cur.expectedNext),
			"err", "decode_failed",
			"seq", u32s(msg.Seq),
			"off", u32s(msg.Off),
			"data_len", u32s(uint32(len(msg.Data))),
			"data_head", textPreview(msg.Data),
			"data_tail", textTailPreview(msg.Data),
		)
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "decode_failed")
		return
	}
	if uint32(len(raw)) != msg.N || msg.N == 0 {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_need sent",
			"id", cur.meta.ID,
			"next", u32s(cur.expectedNext),
			"err", "size_mismatch",
			"seq", u32s(msg.Seq),
			"off", u32s(msg.Off),
			"n", u32s(msg.N),
			"data_len", u32s(uint32(len(msg.Data))),
			"decoded", u32s(uint32(len(raw))),
			"data_head", textPreview(msg.Data),
			"data_tail", textTailPreview(msg.Data),
		)
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "size_mismatch")
		return
	}
	if crc32Hex(raw) != lowerHex(msg.CRC32) {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_need sent",
			"id", cur.meta.ID,
			"next", u32s(cur.expectedNext),
			"err", "bad_crc",
			"seq", u32s(msg.Seq),
		)
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "bad_crc")
		return
	}
	if cur.bytesWritten+uint32(len(raw)) > cur.meta.Size {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_need sent",
			"id", cur.meta.ID,
			"next", u32s(cur.expectedNext),
			"err", "size_mismatch",
			"bytes_written", u32s(cur.bytesWritten),
			"raw_len", u32s(uint32(len(raw))),
			"total", u32s(cur.meta.Size),
		)
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "size_mismatch")
		return
	}
	if err := cur.sink.WriteChunk(msg.Seq, msg.Off, raw); err != nil {
		s.logKV("transfer write failed", "err", err.Error())
		_ = cur.sink.Abort(err.Error())
		s.clearTransfer()
		s.sendTransferDone(cur.meta.ID, false, transferInfo{}, err.Error())
		return
	}
	_, _ = cur.hasher.Write(raw)
	cur.expectedNext++
	cur.bytesWritten += uint32(len(raw))
	cur.chunksSeen++
	if cur.chunksSeen == 1 || (cur.chunksSeen%transferProgressLogEvery) == 0 {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_chunk accepted",
			"id", cur.meta.ID,
			"seq", u32s(msg.Seq),
			"off", u32s(msg.Off),
			"n", u32s(msg.N),
			"data_len", u32s(uint32(len(msg.Data))),
			"bytes_written", u32s(cur.bytesWritten),
		)
	}
	raw = nil
	runtime.GC()
	s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "")
}

func (s *session) onTransferCommit(msg *protoMsg) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.ID {
		s.logKV("xfer_commit dropped", "id", msg.ID)
		return
	}
	if msg.Size != cur.meta.Size || cur.bytesWritten != cur.meta.Size {
		println(
			"[fabric]", "sid", s.localSID,
			"xfer_commit failed",
			"id", cur.meta.ID,
			"err", "size_mismatch",
			"bytes_written", u32s(cur.bytesWritten),
			"msg_size", u32s(msg.Size),
			"meta_size", u32s(cur.meta.Size),
		)
		_ = cur.sink.Abort("size_mismatch")
		s.clearTransfer()
		s.sendTransferDone(cur.meta.ID, false, transferInfo{}, "size_mismatch")
		return
	}
	if lowerHex(msg.SHA256) != cur.meta.SHA256 || sha256Hex(cur.hasher) != cur.meta.SHA256 {
		println("[fabric]", "sid", s.localSID, "xfer_commit failed", "id", cur.meta.ID, "err", "sha256_mismatch")
		_ = cur.sink.Abort("sha256_mismatch")
		s.clearTransfer()
		s.sendTransferDone(cur.meta.ID, false, transferInfo{}, "sha256_mismatch")
		return
	}
	info, err := cur.sink.Commit()
	if err != nil {
		s.logKV("transfer commit failed", "err", err.Error())
		_ = cur.sink.Abort(err.Error())
		s.clearTransfer()
		s.sendTransferDone(cur.meta.ID, false, transferInfo{}, err.Error())
		return
	}
	sink := cur.sink
	id := cur.meta.ID
	s.clearTransfer()
	println(
		"[fabric]", "sid", s.localSID,
		"xfer_commit accepted",
		"id", id,
		"bytes_written", u32s(info.BytesWritten),
	)
	if !s.sendTransferDone(id, true, info, "") {
		return
	}
	time.Sleep(postTransferDoneSettle)
	if err := sink.Apply(); err != nil {
		s.logKV("transfer apply failed", "err", err.Error())
		return
	}
	println("[fabric]", "sid", s.localSID, "transfer apply ok", "id", id)
}

func (s *session) onTransferAbort(msg *protoMsg) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.ID {
		s.logKV("xfer_abort dropped", "id", msg.ID)
		return
	}
	reason := msg.Reason
	if reason == "" {
		reason = "remote_abort"
	}
	if err := cur.sink.Abort(reason); err != nil {
		s.logKV("transfer abort failed", "err", err.Error())
	}
	println("[fabric]", "sid", s.localSID, "xfer_abort received", "id", cur.meta.ID, "reason", reason)
	s.clearTransfer()
}

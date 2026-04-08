package fabric

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"strings"
	"time"
)

const postTransferDoneSettle = 10 * time.Millisecond

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
	s.sendTransferReady(meta.ID, true, readyNext(0), "")
}

func (s *session) onTransferChunk(msg *protoMsg) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.ID {
		s.logKV("xfer_chunk dropped", "id", msg.ID)
		return
	}
	if msg.Seq != cur.expectedNext {
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "unexpected_seq")
		return
	}
	if msg.Off != cur.bytesWritten {
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "unexpected_offset")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(msg.Data)
	if err != nil {
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "decode_failed")
		return
	}
	if uint32(len(raw)) != msg.N || msg.N == 0 {
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "size_mismatch")
		return
	}
	if crc32Hex(raw) != lowerHex(msg.CRC32) {
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "bad_crc")
		return
	}
	if cur.bytesWritten+uint32(len(raw)) > cur.meta.Size {
		s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "size_mismatch")
		return
	}
	if err := cur.sink.WriteChunk(msg.Seq, msg.Off, raw); err != nil {
		_ = cur.sink.Abort(err.Error())
		s.clearTransfer()
		s.sendTransferDone(cur.meta.ID, false, transferInfo{}, err.Error())
		return
	}
	_, _ = cur.hasher.Write(raw)
	cur.expectedNext++
	cur.bytesWritten += uint32(len(raw))
	cur.chunksSeen++
	s.sendTransferNeed(cur.meta.ID, cur.expectedNext, "")
}

func (s *session) onTransferCommit(msg *protoMsg) {
	cur := s.incomingTransfer
	if cur == nil || cur.meta.ID != msg.ID {
		s.logKV("xfer_commit dropped", "id", msg.ID)
		return
	}
	if msg.Size != cur.meta.Size || cur.bytesWritten != cur.meta.Size {
		_ = cur.sink.Abort("size_mismatch")
		s.clearTransfer()
		s.sendTransferDone(cur.meta.ID, false, transferInfo{}, "size_mismatch")
		return
	}
	if lowerHex(msg.SHA256) != cur.meta.SHA256 || sha256Hex(cur.hasher) != cur.meta.SHA256 {
		_ = cur.sink.Abort("sha256_mismatch")
		s.clearTransfer()
		s.sendTransferDone(cur.meta.ID, false, transferInfo{}, "sha256_mismatch")
		return
	}
	info, err := cur.sink.Commit()
	if err != nil {
		_ = cur.sink.Abort(err.Error())
		s.clearTransfer()
		s.sendTransferDone(cur.meta.ID, false, transferInfo{}, err.Error())
		return
	}
	sink := cur.sink
	id := cur.meta.ID
	s.clearTransfer()
	if !s.sendTransferDone(id, true, info, "") {
		return
	}
	time.Sleep(postTransferDoneSettle)
	if err := sink.Apply(); err != nil {
		s.logKV("transfer apply failed", "err", err.Error())
	}
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
	s.clearTransfer()
}

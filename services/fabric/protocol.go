package fabric

import (
	"encoding/json"
	"strconv"
)

// ---- Wire message type identifiers ----
//
// Wire schema mirrors ../docs/updating.md and
// devicecode-lua/src/services/fabric/protocol.lua. The frame discriminator is
// "type". Replies carry {id, ok, payload, err}. Transfers use explicit
// digest_alg/digest fields and required per-chunk chunk_digest.

const (
	protocolName = "fabric-jsonl/1"
	digestAlg    = "xxhash32"

	msgHello      = "hello"
	msgHelloAck   = "hello_ack"
	msgPing       = "ping"
	msgPong       = "pong"
	msgPub        = "pub"
	msgUnretain   = "unretain"
	msgCall       = "call"
	msgReply      = "reply"
	msgXferBegin  = "xfer_begin"
	msgXferReady  = "xfer_ready"
	msgXferChunk  = "xfer_chunk"
	msgXferNeed   = "xfer_need"
	msgXferCommit = "xfer_commit"
	msgXferDone   = "xfer_done"
	msgXferAbort  = "xfer_abort"
)

// ---- Wire message structs ----

type protoHello struct {
	Type     string          `json:"type"`
	Proto    string          `json:"proto"`
	SID      string          `json:"sid"`
	Node     string          `json:"node"`
	Identity json.RawMessage `json:"identity,omitempty"`
	Auth     json.RawMessage `json:"auth,omitempty"`
}

type protoHelloAck struct {
	Type     string          `json:"type"`
	Proto    string          `json:"proto"`
	SID      string          `json:"sid"`
	Node     string          `json:"node"`
	Identity json.RawMessage `json:"identity,omitempty"`
	Auth     json.RawMessage `json:"auth,omitempty"`
}

type protoPing struct {
	Type string `json:"type"`
	SID  string `json:"sid,omitempty"`
}

type protoPong struct {
	Type string `json:"type"`
	SID  string `json:"sid,omitempty"`
}

type protoPub struct {
	Type    string          `json:"type"`
	Topic   []string        `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	Retain  bool            `json:"retain"`
}

type protoUnretain struct {
	Type  string   `json:"type"`
	Topic []string `json:"topic"`
}

type protoCall struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Topic   []string        `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

// protoReply mirrors Lua's reply frame: {type, id, ok, payload, err}. The Go
// field for the correlation id keeps the name "Corr" for readability — the
// wire spelling is "id" because the reply correlates to a prior call.id.
type protoReply struct {
	Type    string          `json:"type"`
	Corr    string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Err     string          `json:"err,omitempty"`
}

// protoXferBegin starts an incoming transfer to a named target. The only
// supported digest for fabric-jsonl/1 is xxhash32 seed 0, lower-hex.
type protoXferBegin struct {
	Type      string          `json:"type"`
	XferID    string          `json:"xfer_id"`
	Target    string          `json:"target"`
	Size      uint32          `json:"size"`
	DigestAlg string          `json:"digest_alg"`
	Digest    string          `json:"digest"`
	Meta      json.RawMessage `json:"meta,omitempty"`
}

// protoXferReady (control) carries only xfer_id; success/failure is implicit
// (failure is signalled via xfer_abort).
type protoXferReady struct {
	Type   string `json:"type"`
	XferID string `json:"xfer_id"`
}

// protoXferChunk carries unpadded base64url data plus a required xxhash32
// digest over the raw decoded chunk bytes.
type protoXferChunk struct {
	Type        string `json:"type"`
	XferID      string `json:"xfer_id"`
	Offset      uint32 `json:"offset"`
	Data        string `json:"data"`
	ChunkDigest string `json:"chunk_digest"`
}

// protoXferNeed (control) acks the MCU's expected next byte offset.
type protoXferNeed struct {
	Type   string `json:"type"`
	XferID string `json:"xfer_id"`
	Next   uint32 `json:"next"`
}

// protoXferCommit repeats the whole-object digest so begin/commit/streamed
// content can be reconciled before the target accepts the object.
type protoXferCommit struct {
	Type      string `json:"type"`
	XferID    string `json:"xfer_id"`
	Size      uint32 `json:"size"`
	DigestAlg string `json:"digest_alg"`
	Digest    string `json:"digest"`
}

// protoXferDone (control) carries only xfer_id; failure is signalled via
// xfer_abort.
type protoXferDone struct {
	Type   string `json:"type"`
	XferID string `json:"xfer_id"`
}

// protoXferAbort (control) carries xfer_id plus an optional err string.
type protoXferAbort struct {
	Type   string `json:"type"`
	XferID string `json:"xfer_id"`
	Err    string `json:"err,omitempty"`
}

// ---- codec helpers ----

// marshal returns compact JSON with a trailing newline.
// Panics on encode failure (should be unreachable for wire structs).
func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("fabric: marshal: " + err.Error())
	}
	return append(b, '\n')
}

// marshalHelloAck returns a compact hello_ack frame without using reflection.
func marshalHelloAck(sid, node string) []byte {
	b := make([]byte, 0, 96)
	b = append(b, `{"type":"hello_ack","proto":"fabric-jsonl/1","sid":"`...)
	b = appendJSONString(b, sid)
	b = append(b, `","node":"`...)
	b = appendJSONString(b, node)
	b = append(b, `"}`...)
	return append(b, '\n')
}

func marshalPing(sid string) []byte { return marshalSIDControl(msgPing, sid) }
func marshalPong(sid string) []byte { return marshalSIDControl(msgPong, sid) }

func marshalSIDControl(typ, sid string) []byte {
	b := make([]byte, 0, 48+len(sid))
	b = append(b, `{"type":"`...)
	b = appendJSONString(b, typ)
	b = append(b, `","sid":"`...)
	b = appendJSONString(b, sid)
	b = append(b, `"}`...)
	return append(b, '\n')
}

func marshalReplyErr(id, errText string) []byte {
	b := make([]byte, 0, 64+len(id)+len(errText))
	b = append(b, `{"type":"reply","id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","ok":false,"err":"`...)
	b = appendJSONString(b, errText)
	b = append(b, `"}`...)
	return append(b, '\n')
}

func marshalReplyOKRaw(id string, payload json.RawMessage) []byte {
	b := make([]byte, 0, 48+len(id)+len(payload))
	b = append(b, `{"type":"reply","id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","ok":true`...)
	if len(payload) > 0 {
		b = append(b, `,"payload":`...)
		b = append(b, payload...)
	}
	b = append(b, `}`...)
	return append(b, '\n')
}

func marshalXferReady(id string) []byte { return marshalXferControl(msgXferReady, id, 0, false, "") }
func marshalXferNeed(id string, next uint32) []byte {
	return marshalXferControl(msgXferNeed, id, next, true, "")
}
func marshalXferDone(id string) []byte { return marshalXferControl(msgXferDone, id, 0, false, "") }
func marshalXferAbort(id, reason string) []byte {
	return marshalXferControl(msgXferAbort, id, 0, false, reason)
}

func marshalXferControl(typ, id string, next uint32, hasNext bool, errText string) []byte {
	b := make([]byte, 0, 80+len(id)+len(errText))
	b = append(b, `{"type":"`...)
	b = appendJSONString(b, typ)
	b = append(b, `","xfer_id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `"`...)
	if hasNext {
		b = append(b, `,"next":`...)
		b = strconv.AppendUint(b, uint64(next), 10)
	}
	if errText != "" {
		b = append(b, `,"err":"`...)
		b = appendJSONString(b, errText)
		b = append(b, `"`...)
	}
	b = append(b, `}`...)
	return append(b, '\n')
}

func appendJSONString(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' {
			dst = append(dst, '\\')
		}
		dst = append(dst, c)
	}
	return dst
}

// protoTopRaw returns the complete top-level JSON value for field.
func protoTopRaw(line []byte, field string) (json.RawMessage, bool) {
	i, ok := findTopJSONValue(line, field)
	if !ok {
		return nil, false
	}
	end, ok := skipJSONValue(line, i)
	if !ok || end < i || end > len(line) {
		return nil, false
	}
	out := make(json.RawMessage, end-i)
	copy(out, line[i:end])
	return out, true
}

func protoTopUint32(line []byte, field string) (uint32, bool) {
	i, ok := findTopJSONValue(line, field)
	if !ok || i >= len(line) || line[i] < '0' || line[i] > '9' {
		return 0, false
	}
	var v uint32
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		d := uint32(line[i] - '0')
		if v > (1<<32-1-d)/10 {
			return 0, false
		}
		v = v*10 + d
		i++
	}
	return v, true
}

func findTopJSONValue(line []byte, field string) (int, bool) {
	n := len(line)
	i := skipJSONSpace(line, 0)
	if i >= n || line[i] != '{' {
		return 0, false
	}
	i++
	for {
		i = skipJSONSpace(line, i)
		if i >= n {
			return 0, false
		}
		switch line[i] {
		case '}':
			return 0, false
		case ',':
			i++
			continue
		}
		if line[i] != '"' {
			return 0, false
		}
		keyStart := i + 1
		keyEnd, ok := scanJSONString(line, i)
		if !ok {
			return 0, false
		}
		i = keyEnd
		i = skipJSONSpace(line, i)
		if i >= n || line[i] != ':' {
			return 0, false
		}
		i++
		i = skipJSONSpace(line, i)
		if i >= n {
			return 0, false
		}
		if jsonKeyEquals(line[keyStart:keyEnd-1], field) {
			return i, true
		}
		i, ok = skipJSONValue(line, i)
		if !ok {
			return 0, false
		}
	}
}

func topFieldsAllowed(line []byte, allowed ...string) bool {
	n := len(line)
	i := skipJSONSpace(line, 0)
	if i >= n || line[i] != '{' {
		return false
	}
	i++
	for {
		i = skipJSONSpace(line, i)
		if i >= n {
			return false
		}
		if line[i] == '}' {
			i++
			i = skipJSONSpace(line, i)
			return i == n
		}
		if line[i] == ',' {
			i++
			continue
		}
		if line[i] != '"' {
			return false
		}
		keyStart := i + 1
		keyEnd, ok := scanJSONString(line, i)
		if !ok {
			return false
		}
		if !jsonKeyInAllowed(line[keyStart:keyEnd-1], allowed) {
			return false
		}
		i = skipJSONSpace(line, keyEnd)
		if i >= n || line[i] != ':' {
			return false
		}
		i++
		i = skipJSONSpace(line, i)
		if i >= n {
			return false
		}
		i, ok = skipJSONValue(line, i)
		if !ok {
			return false
		}
	}
}

func jsonKeyInAllowed(key []byte, allowed []string) bool {
	for _, field := range allowed {
		if jsonKeyEquals(key, field) {
			return true
		}
	}
	return false
}

func decodeHelloFast(line []byte) (protoHello, bool) {
	if !topFieldsAllowed(line, "type", "proto", "sid", "node", "identity", "auth") {
		return protoHello{}, false
	}
	var msg protoHello
	msg.Type = protoTopString(line, "type")
	msg.Proto = protoTopString(line, "proto")
	msg.SID = protoTopString(line, "sid")
	msg.Node = protoTopString(line, "node")
	return msg, msg.Type == msgHello && msg.Proto != "" && msg.SID != ""
}

func decodePingFast(line []byte, want string) (protoPing, bool) {
	if !topFieldsAllowed(line, "type", "sid") {
		return protoPing{}, false
	}
	var msg protoPing
	msg.Type = protoTopString(line, "type")
	msg.SID = protoTopString(line, "sid")
	return msg, msg.Type == want
}

func decodePongFast(line []byte) (protoPong, bool) {
	if !topFieldsAllowed(line, "type", "sid") {
		return protoPong{}, false
	}
	var msg protoPong
	msg.Type = protoTopString(line, "type")
	msg.SID = protoTopString(line, "sid")
	return msg, msg.Type == msgPong
}

func decodeCallFast(line []byte) (protoCall, bool) {
	if !topFieldsAllowed(line, "type", "id", "topic", "payload") {
		return protoCall{}, false
	}
	var msg protoCall
	msg.Type = protoTopString(line, "type")
	msg.ID = protoTopString(line, "id")
	msg.Topic = protoTopStringArray(line, "topic")
	if payload, ok := protoTopRaw(line, "payload"); ok {
		msg.Payload = payload
	}
	return msg, msg.Type == msgCall && msg.ID != "" && len(msg.Topic) > 0
}

func decodeXferBeginFast(line []byte) (protoXferBegin, bool) {
	if !topFieldsAllowed(line, "type", "xfer_id", "target", "size", "digest_alg", "digest", "meta") {
		return protoXferBegin{}, false
	}
	var msg protoXferBegin
	msg.Type = protoTopString(line, "type")
	msg.XferID = protoTopString(line, "xfer_id")
	msg.Target = protoTopString(line, "target")
	msg.Size, _ = protoTopUint32(line, "size")
	msg.DigestAlg = protoTopString(line, "digest_alg")
	msg.Digest = protoTopString(line, "digest")
	if meta, ok := protoTopRaw(line, "meta"); ok {
		msg.Meta = meta
	}
	return msg, msg.Type == msgXferBegin && msg.XferID != ""
}

func decodeXferChunkFast(line []byte) (protoXferChunk, bool) {
	if !topFieldsAllowed(line, "type", "xfer_id", "offset", "data", "chunk_digest") {
		return protoXferChunk{}, false
	}
	var msg protoXferChunk
	msg.Type = protoTopString(line, "type")
	msg.XferID = protoTopString(line, "xfer_id")
	msg.Offset, _ = protoTopUint32(line, "offset")
	msg.Data = protoTopString(line, "data")
	msg.ChunkDigest = protoTopString(line, "chunk_digest")
	return msg, msg.Type == msgXferChunk && msg.XferID != ""
}

func decodeXferCommitFast(line []byte) (protoXferCommit, bool) {
	if !topFieldsAllowed(line, "type", "xfer_id", "size", "digest_alg", "digest") {
		return protoXferCommit{}, false
	}
	var msg protoXferCommit
	msg.Type = protoTopString(line, "type")
	msg.XferID = protoTopString(line, "xfer_id")
	msg.Size, _ = protoTopUint32(line, "size")
	msg.DigestAlg = protoTopString(line, "digest_alg")
	msg.Digest = protoTopString(line, "digest")
	return msg, msg.Type == msgXferCommit && msg.XferID != ""
}

func decodeXferAbortFast(line []byte) (protoXferAbort, bool) {
	if !topFieldsAllowed(line, "type", "xfer_id", "err") {
		return protoXferAbort{}, false
	}
	var msg protoXferAbort
	msg.Type = protoTopString(line, "type")
	msg.XferID = protoTopString(line, "xfer_id")
	msg.Err = protoTopString(line, "err")
	return msg, msg.Type == msgXferAbort && msg.XferID != ""
}

func protoTopStringArray(line []byte, field string) []string {
	i, ok := findTopJSONValue(line, field)
	if !ok || i >= len(line) || line[i] != '[' {
		return nil
	}
	i++
	out := make([]string, 0, 8)
	for {
		i = skipJSONSpace(line, i)
		if i >= len(line) {
			return nil
		}
		if line[i] == ']' {
			return out
		}
		if line[i] == ',' {
			i++
			continue
		}
		if line[i] != '"' {
			return nil
		}
		start := i + 1
		end, ok := scanJSONString(line, i)
		if !ok {
			return nil
		}
		out = append(out, string(line[start:end-1]))
		i = end
	}
}

// protoType extracts the wire-discriminator "type" field from a JSON
// envelope via a depth-aware scan. We avoid json.Unmarshal here because
// TinyGo's reflect path was observed silently leaving the field empty
// for tagged anonymous-struct targets when the envelope had preceding
// sibling keys.
//
// Returns the value of the FIRST top-level (object-depth 1) "type" key,
// ignoring any nested "type" keys inside payload/meta sub-objects —
// e.g. for `{"payload":{"type":"x"},"type":"pub"}` the result is "pub".
// Returns "" if the line isn't a JSON object, the top-level "type" key
// is missing, or its value isn't a string.
func protoType(line []byte) string {
	return protoTopString(line, "type")
}

func protoXferID(line []byte) string {
	return protoTopString(line, "xfer_id")
}

func protoTopString(line []byte, field string) string {
	n := len(line)
	i := skipJSONSpace(line, 0)
	if i >= n || line[i] != '{' {
		return ""
	}
	i++
	for {
		i = skipJSONSpace(line, i)
		if i >= n {
			return ""
		}
		switch line[i] {
		case '}':
			return ""
		case ',':
			i++
			continue
		}
		if line[i] != '"' {
			return ""
		}
		keyStart := i + 1
		keyEnd, ok := scanJSONString(line, i)
		if !ok {
			return ""
		}
		i = keyEnd
		i = skipJSONSpace(line, i)
		if i >= n || line[i] != ':' {
			return ""
		}
		i++
		i = skipJSONSpace(line, i)
		if i >= n {
			return ""
		}
		if jsonKeyEquals(line[keyStart:keyEnd-1], field) {
			if line[i] != '"' {
				return ""
			}
			valStart := i + 1
			valEnd, ok := scanJSONString(line, i)
			if !ok {
				return ""
			}
			return string(line[valStart : valEnd-1])
		}
		i, ok = skipJSONValue(line, i)
		if !ok {
			return ""
		}
	}
}

func jsonKeyEquals(key []byte, field string) bool {
	if len(key) != len(field) {
		return false
	}
	for i := 0; i < len(field); i++ {
		if key[i] != field[i] {
			return false
		}
	}
	return true
}

func skipJSONSpace(line []byte, i int) int {
	for i < len(line) {
		switch line[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// scanJSONString walks an opening-`"` at line[i] to its closing `"`,
// honouring backslash escapes. Returns the index immediately after the
// closing quote, or false on a malformed string.
func scanJSONString(line []byte, i int) (int, bool) {
	n := len(line)
	if i >= n || line[i] != '"' {
		return 0, false
	}
	i++
	for i < n {
		switch line[i] {
		case '\\':
			if i+1 >= n {
				return 0, false
			}
			i += 2
		case '"':
			return i + 1, true
		default:
			i++
		}
	}
	return 0, false
}

// skipJSONValue advances past a value starting at line[i], whatever
// its kind (string, number, bool, null, object, array). Returns the
// index past the value, or false on parse error.
func skipJSONValue(line []byte, i int) (int, bool) {
	n := len(line)
	if i >= n {
		return 0, false
	}
	switch line[i] {
	case '"':
		return scanJSONString(line, i)
	case '{', '[':
		return skipJSONContainer(line, i)
	}
	// number / true / false / null — walk to the next structural byte.
	for i < n {
		switch line[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i, true
		}
		i++
	}
	return i, true
}

// skipJSONContainer walks past a balanced { … } or [ … ] block starting
// at line[i], tracking string state so quoted braces don't disturb the
// depth count. Returns the index past the closing brace, or false.
func skipJSONContainer(line []byte, i int) (int, bool) {
	n := len(line)
	if i >= n {
		return 0, false
	}
	depth := 0
	inString := false
	for i < n {
		c := line[i]
		if inString {
			if c == '\\' {
				if i+1 >= n {
					return 0, false
				}
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			i++
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
		i++
	}
	return 0, false
}

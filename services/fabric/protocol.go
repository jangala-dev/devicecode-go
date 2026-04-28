package fabric

import (
	"encoding/json"

	"devicecode-go/x/strconvx"
)

// ---- Wire message type identifiers ----
//
// Wire schema mirrors devicecode-lua/src/services/fabric/protocol.lua at
// update-migration tip (commit 2c88090). The frame discriminator field is
// "type" (not "t"). Reply frames carry {id, ok, value, err}. Transfer frames
// use xfer_id/offset/checksum/data with a minimal xfer_chunk shape and
// xxHash32 hex wire integrity (no algorithm field; Lua source treats checksum
// as opaque hex).

const (
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

// protoCaps is carried in hello for forward compatibility. The Lua side
// sends caps but neither side enforces them in v1.
type protoCaps struct {
	Pub  bool `json:"pub,omitempty"`
	Call bool `json:"call,omitempty"`
}

type protoHello struct {
	Type  string     `json:"type"`
	Node  string     `json:"node"`
	Peer  string     `json:"peer"`
	SID   string     `json:"sid"`
	Proto int        `json:"proto,omitempty"`
	Caps  *protoCaps `json:"caps,omitempty"`
}

type protoHelloAck struct {
	Type  string `json:"type"`
	Node  string `json:"node"`
	SID   string `json:"sid,omitempty"`
	Proto int    `json:"proto,omitempty"`
	OK    bool   `json:"ok"`
}

type protoPing struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
	SID  string `json:"sid,omitempty"`
}

type protoPong struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
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
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Topic     []string        `json:"topic"`
	Payload   json.RawMessage `json:"payload"`
	TimeoutMs int             `json:"timeout_ms"`
}

// protoReply mirrors Lua's reply frame: {type, id, ok, value, err}. The Go
// field for the correlation id keeps the name "Corr" for readability — the
// wire spelling is "id" because the reply correlates to a prior call.id.
type protoReply struct {
	Type  string          `json:"type"`
	Corr  string          `json:"id"`
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Err   string          `json:"err,omitempty"`
}

// protoXferBegin (control lane) — required fields per protocol.lua
// validate_control: xfer_id, size, checksum (xxHash32 hex). meta is
// optional but source-used: transfer_mgr.lua sends it on xfer_begin and
// later does conn:call(meta.receiver, …) before xfer_done. Preserve the
// blob opaquely so fabric-update's receiver can pull meta.receiver out.
type protoXferBegin struct {
	Type     string          `json:"type"`
	XferID   string          `json:"xfer_id"`
	Size     uint32          `json:"size"`
	Checksum string          `json:"checksum"`
	Meta     json.RawMessage `json:"meta,omitempty"`
}

// protoXferReady (control) carries only xfer_id; success/failure is implicit
// (failure is signalled via xfer_abort).
type protoXferReady struct {
	Type   string `json:"type"`
	XferID string `json:"xfer_id"`
}

// protoXferChunk (bulk) — minimal {xfer_id, offset, data}. No chunk-level
// checksum, no sequence number; ack is by byte offset via xfer_need.next.
type protoXferChunk struct {
	Type   string `json:"type"`
	XferID string `json:"xfer_id"`
	Offset uint32 `json:"offset"`
	Data   string `json:"data"`
}

// protoXferNeed (control) acks the receiver's expected next byte offset.
type protoXferNeed struct {
	Type   string `json:"type"`
	XferID string `json:"xfer_id"`
	Next   uint32 `json:"next"`
}

// protoXferCommit (control) carries the same wire-integrity shape as
// xfer_begin: xfer_id, size, checksum (xxHash32 hex over the payload bytes).
type protoXferCommit struct {
	Type     string `json:"type"`
	XferID   string `json:"xfer_id"`
	Size     uint32 `json:"size"`
	Checksum string `json:"checksum"`
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
//
// DEBUG: every bail path emits a one-line `protoType bail` print so we
// can see on hardware exactly where the scanner gives up. This noise
// will be removed once the hardware bring-up is complete.
func protoType(line []byte) string {
	n := len(line)
	i := skipJSONSpace(line, 0)
	if i >= n || line[i] != '{' {
		debugBail("no_opening_brace", line, i)
		return ""
	}
	i++
	for {
		i = skipJSONSpace(line, i)
		if i >= n {
			debugBail("eof_after_brace", line, i)
			return ""
		}
		switch line[i] {
		case '}':
			debugBail("close_before_type", line, i)
			return ""
		case ',':
			i++
			continue
		}
		if line[i] != '"' {
			debugBail("non_quote_at_key_start", line, i)
			return ""
		}
		keyStart := i + 1
		keyEnd, ok := scanJSONString(line, i)
		if !ok {
			debugBail("scanstring_key_failed", line, i)
			return ""
		}
		i = keyEnd
		i = skipJSONSpace(line, i)
		if i >= n || line[i] != ':' {
			debugBail("missing_colon_after_key", line, i)
			return ""
		}
		i++
		i = skipJSONSpace(line, i)
		if i >= n {
			debugBail("eof_after_colon", line, i)
			return ""
		}
		isType := keyEnd-1-keyStart == 4 &&
			line[keyStart] == 't' && line[keyStart+1] == 'y' &&
			line[keyStart+2] == 'p' && line[keyStart+3] == 'e'
		if isType {
			if line[i] != '"' {
				debugBail("type_value_not_string", line, i)
				return ""
			}
			valStart := i + 1
			valEnd, ok := scanJSONString(line, i)
			if !ok {
				debugBail("scanstring_value_failed", line, i)
				return ""
			}
			return string(line[valStart : valEnd-1])
		}
		i, ok = skipJSONValue(line, i)
		if !ok {
			debugBail("skipvalue_failed", line, i)
			return ""
		}
	}
}

func debugBail(where string, line []byte, i int) {
	preview := line
	if len(preview) > 96 {
		preview = preview[:96]
	}
	println("[fabric-debug] protoType bail at", where,
		"i=", strconvx.Itoa(i), "len=", strconvx.Itoa(len(line)),
		"head=", string(preview))
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

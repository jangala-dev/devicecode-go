package fabric

import (
	"bytes"
	"encoding/json"
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

// protoType extracts the "type" field from a JSON line via a manual
// byte scan rather than json.Unmarshal. TinyGo's encoding/json reflect
// path was observed silently dropping the field for envelopes with
// preceding sibling keys (e.g. {"sid":"…","node":"…","type":"hello"})
// during real-CM5 traffic — frames parsed cleanly under standard Go
// in tests but came back with Type="" on hardware. This scan finds the
// first top-level "type":"…" pair, tolerates whitespace, and assumes
// the value contains no escape sequences (true for every wire type
// constant defined above).
func protoType(line []byte) string {
	const key = `"type"`
	rest := line
	for {
		idx := bytes.Index(rest, []byte(key))
		if idx < 0 {
			return ""
		}
		// Reject matches inside another string value (e.g. someone
		// publishing a payload that contains the literal "type").
		// Simple heuristic: the byte preceding the key must be one of
		// '{', ',' or whitespace at the top level. Good enough for our
		// flat envelopes.
		if idx > 0 {
			b := rest[idx-1]
			if b != '{' && b != ',' && b != ' ' && b != '\t' && b != '\n' && b != '\r' {
				rest = rest[idx+len(key):]
				continue
			}
		}
		v := skipColonAndQuote(rest[idx+len(key):])
		if v == nil {
			return ""
		}
		end := bytes.IndexByte(v, '"')
		if end < 0 {
			return ""
		}
		return string(v[:end])
	}
}

// skipColonAndQuote consumes the `:` plus an opening `"` (with any
// surrounding whitespace) and returns the slice starting at the first
// byte of the value's contents. Returns nil if the shape is wrong.
func skipColonAndQuote(s []byte) []byte {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	if i >= len(s) || s[i] != ':' {
		return nil
	}
	i++
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	if i >= len(s) || s[i] != '"' {
		return nil
	}
	return s[i+1:]
}

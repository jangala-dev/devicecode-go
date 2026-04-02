package fabric

import "encoding/json"

// ---- Wire message type identifiers (fabric.md §4) ----

const (
	msgHello    = "hello"
	msgHelloAck = "hello_ack"
	msgPing     = "ping"
	msgPong     = "pong"
	msgPub      = "pub"
	msgUnretain = "unretain"
	msgCall     = "call"
	msgReply    = "reply"
)

// ---- Wire message structs ----

// protoCaps is carried in hello for forward compatibility. The Lua side
// sends caps but neither side enforces them in v1.
type protoCaps struct {
	Pub  bool `json:"pub,omitempty"`
	Call bool `json:"call,omitempty"`
}

type protoHello struct {
	T     string     `json:"t"`
	Node  string     `json:"node"`
	Peer  string     `json:"peer"`
	SID   string     `json:"sid"`
	Proto int        `json:"proto,omitempty"`
	Caps  *protoCaps `json:"caps,omitempty"`
}

type protoHelloAck struct {
	T     string `json:"t"`
	Node  string `json:"node"`
	SID   string `json:"sid,omitempty"`
	Proto int    `json:"proto,omitempty"`
	OK    bool   `json:"ok"`
}

type protoPing struct {
	T   string `json:"t"`
	TS  int64  `json:"ts"`
	SID string `json:"sid,omitempty"`
}

type protoPong struct {
	T   string `json:"t"`
	TS  int64  `json:"ts"`
	SID string `json:"sid,omitempty"`
}

// Not wired yet — defined for forward compatibility.

type protoPub struct {
	T       string          `json:"t"`
	Topic   []string        `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	Retain  bool            `json:"retain"`
}

type protoUnretain struct {
	T     string   `json:"t"`
	Topic []string `json:"topic"`
}

type protoCall struct {
	T         string          `json:"t"`
	ID        string          `json:"id"`
	Topic     []string        `json:"topic"`
	Payload   json.RawMessage `json:"payload"`
	TimeoutMs int             `json:"timeout_ms"`
}

type protoReply struct {
	T       string          `json:"t"`
	Corr    string          `json:"corr"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Err     string          `json:"err,omitempty"`
}

// protoMsg is a union struct for single-pass unmarshal in dispatch.
// Fields are the superset of all message types. Only the fields
// relevant to the T value are populated; the rest are zero.
type protoMsg struct {
	T         string          `json:"t"`
	Node      string          `json:"node,omitempty"`
	Peer      string          `json:"peer,omitempty"`
	SID       string          `json:"sid,omitempty"`
	Proto     int             `json:"proto,omitempty"`
	OK        bool            `json:"ok,omitempty"`
	Caps      *protoCaps      `json:"caps,omitempty"`
	TS        int64           `json:"ts,omitempty"`
	Topic     []string        `json:"topic,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Retain    bool            `json:"retain,omitempty"`
	ID        string          `json:"id,omitempty"`
	Corr      string          `json:"corr,omitempty"`
	TimeoutMs int             `json:"timeout_ms,omitempty"`
	Err       string          `json:"err,omitempty"`
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

// protoType extracts the "t" field from a JSON line.
func protoType(line []byte) string {
	var env struct {
		T string `json:"t"`
	}
	json.Unmarshal(line, &env)
	return env.T
}

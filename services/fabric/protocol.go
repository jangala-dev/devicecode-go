package fabric

import "encoding/json"

// ---- Wire message type identifiers (fabric.md §4) ----

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

type protoXferBegin struct {
	T        string          `json:"t"`
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Name     string          `json:"name"`
	Format   string          `json:"format"`
	Enc      string          `json:"enc"`
	Size     uint32          `json:"size"`
	ChunkRaw uint32          `json:"chunk_raw"`
	Chunks   uint32          `json:"chunks"`
	SHA256   string          `json:"sha256"`
	Meta     json.RawMessage `json:"meta,omitempty"`
}

type protoXferReady struct {
	T    string  `json:"t"`
	ID   string  `json:"id"`
	OK   bool    `json:"ok"`
	Next *uint32 `json:"next,omitempty"`
	Err  string  `json:"err,omitempty"`
}

type protoXferChunk struct {
	T     string `json:"t"`
	ID    string `json:"id"`
	Seq   uint32 `json:"seq"`
	Off   uint32 `json:"off"`
	N     uint32 `json:"n"`
	CRC32 string `json:"crc32"`
	Data  string `json:"data"`
}

type protoXferNeed struct {
	T    string `json:"t"`
	ID   string `json:"id"`
	Next uint32 `json:"next"`
	Err  string `json:"err,omitempty"`
}

type protoXferCommit struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	Size   uint32 `json:"size"`
	SHA256 string `json:"sha256"`
}

type protoXferDone struct {
	T    string          `json:"t"`
	ID   string          `json:"id"`
	OK   bool            `json:"ok"`
	Info json.RawMessage `json:"info,omitempty"`
	Err  string          `json:"err,omitempty"`
}

type protoXferAbort struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
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

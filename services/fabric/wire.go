package fabric

import "encoding/json"

// ---- Wire message types (fabric.md §4) ----

type wireCaps struct {
	Pub  bool `json:"pub,omitempty"`
	Call bool `json:"call,omitempty"`
}

type wireHello struct {
	T     string    `json:"t"`
	Node  string    `json:"node"`
	Peer  string    `json:"peer"`
	SID   string    `json:"sid"`
	Proto int       `json:"proto,omitempty"`
	Caps  *wireCaps `json:"caps,omitempty"`
}

type wireHelloAck struct {
	T     string `json:"t"`
	Node  string `json:"node"`
	SID   string `json:"sid,omitempty"`
	Proto int    `json:"proto,omitempty"`
	OK    bool   `json:"ok"`
}

type wirePing struct {
	T   string `json:"t"`
	TS  int64  `json:"ts"`
	SID string `json:"sid,omitempty"`
}

type wirePong struct {
	T   string `json:"t"`
	TS  int64  `json:"ts"`
	SID string `json:"sid,omitempty"`
}

// Not wired yet — defined for forward compatibility.

type wirePub struct {
	T       string          `json:"t"`
	Topic   []string        `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	Retain  bool            `json:"retain"`
}

type wireUnretain struct {
	T     string   `json:"t"`
	Topic []string `json:"topic"`
}

type wireCall struct {
	T         string          `json:"t"`
	ID        string          `json:"id"`
	Topic     []string        `json:"topic"`
	Payload   json.RawMessage `json:"payload"`
	TimeoutMs int             `json:"timeout_ms"`
}

type wireReply struct {
	T       string          `json:"t"`
	Corr    string          `json:"corr"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Err     string          `json:"err,omitempty"`
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

// wireType extracts the "t" field from a JSON line.
func wireType(line []byte) string {
	var env struct {
		T string `json:"t"`
	}
	json.Unmarshal(line, &env)
	return env.T
}

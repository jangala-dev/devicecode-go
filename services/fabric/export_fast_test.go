package fabric

import (
	"encoding/json"
	"testing"

	"devicecode-go/bus"
)

type fastPayloadForTest struct{}

func (fastPayloadForTest) AppendJSON(dst []byte) []byte {
	return append(dst, `{"n":7}`...)
}

func TestMarshalPubAppendJSONProducesValidPub(t *testing.T) {
	frame, ok := marshalPubAppendJSON(exportItem{
		topic:    bus.T("state", "self", "runtime", "memory"),
		payload:  fastPayloadForTest{},
		retained: true,
	}, fastPayloadForTest{})
	if !ok {
		t.Fatalf("marshalPubAppendJSON did not accept state/self topic")
	}
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		t.Fatalf("frame missing newline: %q", string(frame))
	}
	var got struct {
		Type    string          `json:"type"`
		Topic   []string        `json:"topic"`
		Payload json.RawMessage `json:"payload"`
		Retain  bool            `json:"retain"`
	}
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatalf("invalid JSON frame: %v\n%s", err, string(frame))
	}
	if got.Type != "pub" || !got.Retain || string(got.Payload) != `{"n":7}` {
		t.Fatalf("unexpected frame: type=%q retain=%v payload=%s", got.Type, got.Retain, got.Payload)
	}
	wantTopic := []string{"state", "self", "runtime", "memory"}
	if len(got.Topic) != len(wantTopic) {
		t.Fatalf("topic length = %d, want %d", len(got.Topic), len(wantTopic))
	}
	for i := range wantTopic {
		if got.Topic[i] != wantTopic[i] {
			t.Fatalf("topic[%d] = %q, want %q", i, got.Topic[i], wantTopic[i])
		}
	}
}

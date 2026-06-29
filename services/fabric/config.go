package fabric

import "encoding/json"

// decodePayload normalises whatever shape the bus delivered into a
// reasonable Go value for the reply path. The wire delivers
// json.RawMessage; in-process callers may pass already-typed values.
// Used by session.onReply when forwarding RPC replies onto the
// originating Request's reply path.
//
// This file intentionally contains only reply-payload decoding; legacy
// config/device and rpc/hal/dump glue is no longer part of the MCU contract.
func decodePayload(payload any) any {
	switch v := payload.(type) {
	case nil:
		return nil
	case json.RawMessage:
		if len(v) == 0 {
			return nil
		}
		var out any
		if err := json.Unmarshal(v, &out); err == nil {
			return out
		}
		return []byte(v)
	case []byte:
		if len(v) == 0 {
			return nil
		}
		var out any
		if err := json.Unmarshal(v, &out); err == nil {
			return out
		}
		cp := make([]byte, len(v))
		copy(cp, v)
		return cp
	default:
		return v
	}
}

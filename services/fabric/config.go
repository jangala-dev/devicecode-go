package fabric

import (
	"encoding/json"

	"devicecode-go/types"
)

// decodeHALConfig extracts a HALConfig from an arbitrary payload,
// normalizing Lua empty-table encoding ({} → []) for known slice fields.
func decodeHALConfig(payload any) (types.HALConfig, string) {
	switch v := payload.(type) {
	case types.HALConfig:
		return v, ""
	case *types.HALConfig:
		if v == nil {
			return types.HALConfig{}, "nil_hal_config"
		}
		return *v, ""
	case json.RawMessage:
		return decodeHALConfigBytes(v)
	case []byte:
		return decodeHALConfigBytes(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return types.HALConfig{}, "payload_marshal_failed: " + err.Error()
		}
		return decodeHALConfigBytes(b)
	}
}

func decodeHALConfigBytes(b []byte) (types.HALConfig, string) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return types.HALConfig{}, "json_unmarshal_failed: " + err.Error() + "; raw=" + truncateRawJSON(b)
	}
	if _, ok := probe["devices"]; !ok {
		return types.HALConfig{}, "missing_devices_field; raw=" + truncateRawJSON(b)
	}

	// Lua encodes empty tables as {} (object) not [] (array).
	// Normalize known slice fields so Go unmarshal accepts them.
	for _, key := range []string{"devices", "pollers"} {
		if raw, ok := probe[key]; ok && len(raw) == 2 && raw[0] == '{' && raw[1] == '}' {
			probe[key] = json.RawMessage("[]")
		}
	}
	fixed, err := json.Marshal(probe)
	if err != nil {
		return types.HALConfig{}, "normalize_failed: " + err.Error()
	}

	var out types.HALConfig
	if err := json.Unmarshal(fixed, &out); err != nil {
		return types.HALConfig{}, "hal_config_unmarshal_failed: " + err.Error() + "; raw=" + truncateRawJSON(fixed)
	}
	return out, ""
}

func decodeHALState(payload any) (types.HALState, bool) {
	switch v := payload.(type) {
	case types.HALState:
		return v, true
	case *types.HALState:
		if v == nil {
			return types.HALState{}, false
		}
		return *v, true
	case json.RawMessage:
		var out types.HALState
		return out, json.Unmarshal(v, &out) == nil
	case []byte:
		var out types.HALState
		return out, json.Unmarshal(v, &out) == nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return types.HALState{}, false
		}
		var out types.HALState
		return out, json.Unmarshal(b, &out) == nil
	}
}

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

func truncateRawJSON(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	const max = 160
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

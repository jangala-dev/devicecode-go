package hal

import "strings"

// Shared helpers used by GPIO code.

func parsePull(v any) Pull {
	switch s := asString(v); s {
	case "up", "UP", "pullup":
		return PullUp
	case "down", "DOWN", "pulldown":
		return PullDown
	default:
		return PullNone
	}
}

func toPullString(p Pull) string {
	switch p {
	case PullUp:
		return "up"
	case PullDown:
		return "down"
	default:
		return "none"
	}
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func edgeToString(e Edge) string {
	switch e {
	case EdgeRising:
		return "rising"
	case EdgeFalling:
		return "falling"
	case EdgeBoth:
		return "both"
	default:
		return "none"
	}
}

// ParseEdge converts a string to an Edge enum.
// Accepts: "rising", "falling", "both", "none" (case-insensitive).
func ParseEdge(s string) Edge {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "rising":
		return EdgeRising
	case "falling":
		return EdgeFalling
	case "both":
		return EdgeBoth
	case "", "none":
		return EdgeNone
	default:
		return EdgeNone
	}
}

// wantBool extracts a boolean from either a map payload (by key) or a scalar.
// Recognises true/false, 1/0, on/off, yes/no (case-insensitive).
func wantBool(src any, key string) bool {
	// If src is a map, look up key first.
	if m, ok := src.(map[string]any); ok {
		if v, ok := m[key]; ok {
			return wantBool(v, "")
		}
		// fall through to return false
	}

	switch v := src.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case int8:
		return v != 0
	case int16:
		return v != 0
	case int32:
		return v != 0
	case int64:
		return v != 0
	case uint:
		return v != 0
	case uint8:
		return v != 0
	case uint16:
		return v != 0
	case uint32:
		return v != 0
	case uint64:
		return v != 0
	case float32:
		return int(v) != 0
	case float64:
		return int(v) != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		switch s {
		case "1", "true", "on", "yes":
			return true
		case "0", "false", "off", "no":
			return false
		default:
			return false
		}
	default:
		return false
	}
}

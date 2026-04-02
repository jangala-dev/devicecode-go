package fabric

import (
	"context"
	"encoding/json"

	"devicecode-go/bus"
	"devicecode-go/types"
)

var (
	tConfigDevice = bus.T("config", "device")
	tConfigHAL    = bus.T("config", "hal")
	tRPCHALDump   = bus.T("rpc", "hal", "dump")
	tHALState     = bus.T("hal", "state")
)

type dumpReply struct {
	OK          bool            `json:"ok"`
	Method      string          `json:"method"`
	Echo        any             `json:"echo,omitempty"`
	HAL         *types.HALState `json:"hal,omitempty"`
	Applied     bool            `json:"applied"`
	ConfigCount int             `json:"config_count,omitempty"`
	ConfigError string          `json:"config_error,omitempty"`
}

// RunBridge connects generic fabric import topics to concrete MCU services.
//
// The current Lua-side config exports `config/mcu -> config/device`, while the
// MCU HAL consumes `config/hal`. This bridge translates matching HAL configs.
// It also exposes a minimal `rpc/hal/dump` endpoint so CM5 proxy calls have a
// real MCU-side responder.
func RunBridge(ctx context.Context, conn *bus.Connection) {
	cfgSub := conn.Subscribe(tConfigDevice)
	dumpSub := conn.Subscribe(tRPCHALDump)
	halStateSub := conn.Subscribe(tHALState)
	defer conn.Unsubscribe(cfgSub)
	defer conn.Unsubscribe(dumpSub)
	defer conn.Unsubscribe(halStateSub)

	var lastHAL *types.HALState
	var appliedConfig bool
	var appliedConfigCount int
	var lastConfigErr string

	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-halStateSub.Channel():
			if !ok || msg == nil {
				return
			}
			if st, ok := decodeHALState(msg.Payload); ok {
				stCopy := st
				lastHAL = &stCopy
			}

		case msg, ok := <-cfgSub.Channel():
			if !ok || msg == nil {
				return
			}
			processConfigDevice(conn, msg, &appliedConfig, &appliedConfigCount, &lastConfigErr)

		case msg, ok := <-dumpSub.Channel():
			if !ok || msg == nil {
				return
			}
			if !msg.CanReply() {
				continue
			}
			if !drainConfigDevice(conn, cfgSub, &appliedConfig, &appliedConfigCount, &lastConfigErr) {
				return
			}
			drainHALState(halStateSub, &lastHAL)
			conn.Reply(msg, dumpReply{
				OK:          true,
				Method:      "dump",
				Echo:        decodePayload(msg.Payload),
				HAL:         lastHAL,
				Applied:     appliedConfig,
				ConfigCount: appliedConfigCount,
				ConfigError: lastConfigErr,
			}, false)
		}
	}
}

func drainConfigDevice(conn *bus.Connection, cfgSub *bus.Subscription, appliedConfig *bool, appliedConfigCount *int, lastConfigErr *string) bool {
	for {
		select {
		case msg, ok := <-cfgSub.Channel():
			if !ok || msg == nil {
				return false
			}
			processConfigDevice(conn, msg, appliedConfig, appliedConfigCount, lastConfigErr)
		default:
			return true
		}
	}
}

func drainHALState(halSub *bus.Subscription, lastHAL **types.HALState) {
	for {
		select {
		case msg, ok := <-halSub.Channel():
			if !ok || msg == nil {
				return
			}
			if st, ok := decodeHALState(msg.Payload); ok {
				stCopy := st
				*lastHAL = &stCopy
			}
		default:
			return
		}
	}
}

func processConfigDevice(conn *bus.Connection, msg *bus.Message, appliedConfig *bool, appliedConfigCount *int, lastConfigErr *string) {
	cfg, err := decodeHALConfig(msg.Payload)
	if err != "" {
		*lastConfigErr = err
		println("[fabric] config/device rejected:", err)
		return
	}
	*appliedConfig = true
	*appliedConfigCount++
	*lastConfigErr = ""
	println("[fabric] config/device bridged to config/hal", *appliedConfigCount, "devices", len(cfg.Devices))
	conn.Publish(conn.NewMessage(tConfigHAL, cfg, true))
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

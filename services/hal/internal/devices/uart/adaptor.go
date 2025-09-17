// services/hal/internal/devices/uart/adaptor.go
package uart

import (
	"context"
	"encoding/base64"
	"time"

	"devicecode-go/services/hal/internal/consts"
	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/halerr"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/services/hal/internal/util"
)

func init() { registry.RegisterBuilder("uart", builder{}) }

type Params struct {
	Baud        uint32 `json:"baud,omitempty"`          // default driver value if zero
	Mode        string `json:"mode,omitempty"`          // "bytes" | "lines"
	MaxFrame    int    `json:"max_frame,omitempty"`     // 16..256 (default 128)
	IdleFlushMS int    `json:"idle_flush_ms,omitempty"` // lines mode: default 100
	EchoTX      bool   `json:"echo_tx,omitempty"`       // publish tx echoes
	// Optional format: defaults to 8N1 if unset and supported.
	DataBits uint8  `json:"databits,omitempty"`
	StopBits uint8  `json:"stopbits,omitempty"`
	Parity   string `json:"parity,omitempty"` // "none"|"even"|"odd"
}

type adaptor struct {
	id   string
	port halcore.UARTPort
}

type builder struct{}

func (builder) Build(in registry.BuildInput) (registry.BuildOutput, error) {
	// Enforce BusRef use for consistency.
	if in.BusRefType != "uart" || in.BusRefID == "" {
		return registry.BuildOutput{}, halerr.ErrMissingBusRef
	}
	u, ok := in.UARTs.ByID(in.BusRefID)
	if !ok {
		return registry.BuildOutput{}, halerr.ErrUnknownBus
	}
	var p Params
	if err := util.DecodeJSON(in.ParamsJSON, &p); err != nil {
		return registry.BuildOutput{}, err
	}

	// Optional format where supported.
	if f, ok := u.(halcore.UARTFormatter); ok {
		if p.Baud > 0 {
			f.SetBaudRate(p.Baud)
		}
		var par uint8
		switch p.Parity {
		case "even":
			par = 1
		case "odd":
			par = 2
		default:
			par = 0
		}
		db := util.ClampInt(int(p.DataBits), 5, 8)
		sb := util.ClampInt(int(p.StopBits), 1, 2)
		_ = f.SetFormat(uint8(db), uint8(sb), par) // best-effort
	}

	ad := &adaptor{id: in.DeviceID, port: u}

	// Register a reader with the service via BuildOutput.UART.
	mode := "bytes"
	if p.Mode == "lines" {
		mode = "lines"
	}
	maxf := util.ClampInt(p.MaxFrame, 16, 256)
	idle := util.ClampInt(p.IdleFlushMS, 0, 1000)

	out := registry.BuildOutput{
		Adaptor: ad,
		UART: &registry.UARTRequest{
			DevID:         in.DeviceID,
			Port:          u,
			Mode:          mode,
			MaxFrame:      maxf,
			IdleFlushMS:   idle,
			PublishTXEcho: p.EchoTX,
		},
	}
	return out, nil
}

func (a *adaptor) ID() string { return a.id }

func (a *adaptor) Capabilities() []halcore.CapInfo {
	return []halcore.CapInfo{
		{
			Kind: consts.KindUART,
			Info: map[string]any{
				"schema_version": 1,
				"driver":         "uart",
			},
		},
	}
}

// UART is stream-oriented; measurement cycle unused.
func (a *adaptor) Trigger(ctx context.Context) (time.Duration, error) {
	return 0, halcore.ErrUnsupported
}
func (a *adaptor) Collect(ctx context.Context) (halcore.Sample, error) {
	return nil, halcore.ErrUnsupported
}

// Controls:
//   - write: {"text":"..."} OR {"data_b64":"..."} → {ok:true,n:int}
//   - set_baud: {"baud":115200}
//   - set_format: {"databits":8,"stopbits":1,"parity":"none|even|odd"}
func (a *adaptor) Control(kind, method string, payload any) (any, error) {
	if kind != consts.KindUART {
		return nil, halcore.ErrUnsupported
	}
	switch method {
	case "write":
		data, ok := decodeWritePayload(payload)
		if !ok {
			return nil, halerr.ErrInvalidPayload
		}
		n, err := a.port.Write(data)
		return map[string]any{"ok": err == nil, "n": n}, err
	case "set_baud":
		if f, ok := a.port.(halcore.UARTFormatter); ok {
			if m, ok := payload.(map[string]any); ok {
				switch v := m["baud"].(type) {
				case int:
					f.SetBaudRate(uint32(v))
					return map[string]any{"ok": true}, nil
				case int64:
					f.SetBaudRate(uint32(v))
					return map[string]any{"ok": true}, nil
				case float64:
					f.SetBaudRate(uint32(v))
					return map[string]any{"ok": true}, nil
				}
			}
			return nil, halerr.ErrInvalidPayload
		}
		return nil, halcore.ErrUnsupported
	case "set_format":
		if f, ok := a.port.(halcore.UARTFormatter); ok {
			m, _ := payload.(map[string]any)
			db := util.ClampInt(intFrom(m, "databits", 8), 5, 8)
			sb := util.ClampInt(intFrom(m, "stopbits", 1), 1, 2)
			var par uint8
			switch strFrom(m, "parity") {
			case "even":
				par = 1
			case "odd":
				par = 2
			default:
				par = 0
			}
			return map[string]any{"ok": true}, f.SetFormat(uint8(db), uint8(sb), par)
		}
		return nil, halcore.ErrUnsupported
	default:
		return nil, halcore.ErrUnsupported
	}
}

func decodeWritePayload(p any) ([]byte, bool) {
	if m, ok := p.(map[string]any); ok {
		if t, ok := m["text"].(string); ok {
			return []byte(t), true
		}
		if s, ok := m["data_b64"].(string); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return b, true
			}
		}
	}
	return nil, false
}
func intFrom(m map[string]any, k string, def int) int {
	if m == nil {
		return def
	}
	switch v := m[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return def
	}
}
func strFrom(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

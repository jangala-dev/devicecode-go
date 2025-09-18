// services/hal/internal/devices/uart/adaptor.go
package uartadpt

import (
	"context"
	"time"

	"devicecode-go/services/hal/internal/consts"
	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/halerr"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/services/hal/internal/util"
	"devicecode-go/types"
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
			Info: types.UARTInfo{
				SchemaVersion: 1,
				Driver:        "uart",
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
		p, ok := payload.(types.UARTWrite)
		if !ok {
			return nil, halerr.ErrInvalidPayload
		}
		n, err := a.port.Write(p.Data)
		return types.UARTWriteReply{OK: err == nil, N: n}, err
	case "set_baud":
		if f, ok := a.port.(halcore.UARTFormatter); ok {
			p, ok := payload.(types.UARTSetBaud)
			if !ok {
				return nil, halerr.ErrInvalidPayload
			}
			f.SetBaudRate(p.Baud)
			return struct{ OK bool }{OK: true}, nil
		}
		return nil, halcore.ErrUnsupported
	case "set_format":
		if f, ok := a.port.(halcore.UARTFormatter); ok {
			p, ok := payload.(types.UARTSetFormat)
			if !ok {
				return nil, halerr.ErrInvalidPayload
			}
			db := util.ClampInt(int(p.DataBits), 5, 8)
			sb := util.ClampInt(int(p.StopBits), 1, 2)
			var par uint8
			switch p.Parity {
			case types.ParityEven:
				par = 1
			case types.ParityOdd:
				par = 2
			default:
				par = 0
			}
			return struct{ OK bool }{OK: true}, f.SetFormat(uint8(db), uint8(sb), par)
		}
		return nil, halcore.ErrUnsupported
	default:
		return nil, halcore.ErrUnsupported
	}
}

// bridge/bridge.go
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"devicecode-go/bus"
)

// -----------------------------------------------------------------------------
// Public entry point
// -----------------------------------------------------------------------------

// Start starts the bridge service. It blocks until ctx is cancelled.
// It listens for JSON config on topic {"config","bridge"} and (re)configures the link.
func Start(ctx context.Context, conn *bus.Connection) {
	s := &Service{
		conn:       conn,
		stateTopic: bus.Topic{"bridge", "state"},
	}
	s.run(ctx)
}

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

// Config is the JSON-encoded configuration expected on "config/bridge".
type Config struct {
	Transport TransportConfig `json:"transport"`

	// Next:
	// Remap rules, allow/deny lists, QoS, priorities, etc.
	// Remap        []RemapRule `json:"remap,omitempty"`
	// PublishAllow []string    `json:"publish_allow,omitempty"`
	// SubscribeReq []string    `json:"subscribe_req,omitempty"`
}

type TransportConfig struct {
	// "uart" (provided here) or other names registered via RegisterTransport.
	Type string      `json:"type"`
	UART *UARTConfig `json:"uart,omitempty"`
}

// UARTConfig carries enough information for an injected TinyGo dialler to open the UART.
// The actual pin mapping and UART instance selection will be handled in UARTDial.
type UARTConfig struct {
	Baud           int `json:"baud"`
	RxPin          int `json:"rx_pin"` // platform-specific numeric IDs (e.g. machine.GPIOxx)
	TxPin          int `json:"tx_pin"`
	ReadTimeoutMS  int `json:"read_timeout_ms,omitempty"` // per read; 0 means blocking
	WriteTimeoutMS int `json:"write_timeout_ms,omitempty"`
	// Optional hardware flow control flags etc. may be added later.
}

// RemapRule example (placeholder for later phases).
type RemapRule struct {
	Match        string `json:"match"`         // e.g. "system/#"
	RemotePrefix string `json:"remote_prefix"` // e.g. "pico/system"
	Direction    string `json:"direction"`     // "up", "down", or "both"
}

// -----------------------------------------------------------------------------
// Service
// -----------------------------------------------------------------------------

type Service struct {
	conn       *bus.Connection
	stateTopic bus.Topic

	mu     sync.Mutex
	curRun context.CancelFunc
	curCfg atomic.Value // stores Config
}

// run waits for config and supervises a single link instance (more in later phases).
func (s *Service) run(ctx context.Context) {
	cfgSub := s.conn.Subscribe(bus.Topic{"config", "bridge"})
	defer s.conn.Unsubscribe(cfgSub)

	s.publishState("idle", "awaiting_config", nil)

	for {
		select {
		case <-ctx.Done():
			s.stopCurrent()
			return
		case msg, ok := <-cfgSub.Channel():
			if !ok {
				s.publishState("error", "config_subscription_closed", nil)
				return
			}
			cfg, err := decodeConfig(msg.Payload)
			if err != nil {
				s.publishState("error", "config_decode_failed", err)
				continue
			}
			s.reconfigure(ctx, cfg)
		}
	}
}

func (s *Service) stopCurrent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curRun != nil {
		s.curRun()
		s.curRun = nil
	}
}

func (s *Service) reconfigure(parent context.Context, cfg Config) {
	s.mu.Lock()
	// Cancel any existing run.
	if s.curRun != nil {
		s.curRun()
		s.curRun = nil
	}
	ctx, cancel := context.WithCancel(parent)
	s.curRun = cancel
	s.mu.Unlock()

	s.curCfg.Store(cfg)
	go s.runLink(ctx, cfg)
}

// -----------------------------------------------------------------------------
// Link supervision and I/O
// -----------------------------------------------------------------------------

func (s *Service) runLink(ctx context.Context, cfg Config) {
	tr, err := newTransport(cfg.Transport)
	if err != nil {
		s.publishState("error", "transport_init_failed", err)
		return
	}

	backoff := backoffSeq(250*time.Millisecond, 5*time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		rwc, err := tr.Open(ctx)
		if err != nil {
			delay := backoff()
			s.publishState("degraded", "dial_failed_retrying", fmt.Errorf("%v (retry in %s)", err, delay))
			if !sleep(ctx, delay) {
				return
			}
			continue
		}

		s.publishState("up", "link_established", nil)
		if err := s.handleLink(ctx, rwc); err != nil {
			_ = rwc.Close()
			delay := backoff()
			s.publishState("degraded", "link_lost_retrying", fmt.Errorf("%v (retry in %s)", err, delay))
			if !sleep(ctx, delay) {
				return
			}
			continue
		}
		// Clean close: restart only on new config.
		return
	}
}

// handleLink owns the active link lifetime.
// TODO: add subscription sync, publish forwarding, retained sync, and request–reply routing.
func (s *Service) handleLink(ctx context.Context, rwc io.ReadWriteCloser) error {
	// Simple heartbeat protocol for scaffolding only.
	rd := newFramedReader(rwc)
	wr := newFramedWriter(rwc)

	// Reader
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		for {
			f, err := rd.ReadFrame()
			if err != nil {
				errCh <- err
				return
			}
			switch f.Type {
			case framePong:
				// Optional: publish RTT etc.
			case framePub, frameSub, frameUnsub, frameAck:
				// TODO: route into local bus with mapping and interest checks.
			default:
				// Unknown; ignore or log via state topic as needed.
			}
		}
	}()

	// Heartbeat + placeholder write loop.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			// Best-effort close.
			_ = wr.WriteFrame(Frame{Type: frameClose})
			return nil
		case err := <-errCh:
			if err != nil {
				return err
			}
			return nil
		case <-tick.C:
			if err := wr.WriteFrame(Frame{Type: framePing}); err != nil {
				return err
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Transport registry
// -----------------------------------------------------------------------------

// Transport is a pluggable link dialler/owner.
type Transport interface {
	Open(ctx context.Context) (io.ReadWriteCloser, error)
	String() string
}

type transportFactory func(TransportConfig) (Transport, error)

var (
	regMu     sync.RWMutex
	registry  = map[string]transportFactory{}
	errNoDial = errors.New("UARTDial not implemented")
)

// RegisterTransport allows external packages to add transports (eg. "ws", "tcp").
func RegisterTransport(name string, f transportFactory) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[name] = f
}

func newTransport(cfg TransportConfig) (Transport, error) {
	regMu.RLock()
	f, ok := registry[cfg.Type]
	regMu.RUnlock()
	if ok {
		return f(cfg)
	}
	switch cfg.Type {
	case "uart":
		return newUARTTransport(cfg)
	default:
		return nil, fmt.Errorf("unknown transport type: %q", cfg.Type)
	}
}

// UARTDial is injected by platform code (eg. in main or a tinygo_uart.go).
// It must open and return an io.ReadWriteCloser over the configured UART.
var UARTDial func(ctx context.Context, u UARTConfig) (io.ReadWriteCloser, error)

// uartTransport implements Transport via an injected dial function.
type uartTransport struct {
	cfg TransportConfig
}

func newUARTTransport(cfg TransportConfig) (Transport, error) {
	if cfg.UART == nil {
		return nil, errors.New("uart transport requires uart config")
	}
	return &uartTransport{cfg: cfg}, nil
}

func (u *uartTransport) Open(ctx context.Context) (io.ReadWriteCloser, error) {
	if UARTDial == nil {
		return nil, errNoDial
	}
	return UARTDial(ctx, *u.cfg.UART)
}

func (u *uartTransport) String() string { return "uart" }

// -----------------------------------------------------------------------------
// Minimal framing (placeholder; replace with CBOR/MsgPack later)
// -----------------------------------------------------------------------------

const (
	framePing  byte = 0x01
	framePong  byte = 0x02
	framePub   byte = 0x10
	frameSub   byte = 0x11
	frameUnsub byte = 0x12
	frameAck   byte = 0x13
	frameClose byte = 0x7f
)

// Frame is a very simple length-prefixed frame.
type Frame struct {
	Type    byte
	Payload []byte
}

type framedReader struct{ r io.Reader }
type framedWriter struct{ w io.Writer }

func newFramedReader(r io.Reader) *framedReader { return &framedReader{r: r} }
func newFramedWriter(w io.Writer) *framedWriter { return &framedWriter{w: w} }

func (fr *framedReader) ReadFrame() (Frame, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(fr.r, hdr[:]); err != nil {
		return Frame{}, err
	}
	typ := hdr[0]
	n := int(hdr[1])<<8 | int(hdr[2])
	var buf []byte
	if n > 0 {
		buf = make([]byte, n)
		if _, err := io.ReadFull(fr.r, buf); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Type: typ, Payload: buf}, nil
}

func (fw *framedWriter) WriteFrame(f Frame) error {
	if len(f.Payload) > 0xFFFF {
		return fmt.Errorf("frame too large: %d", len(f.Payload))
	}
	hdr := []byte{f.Type, byte(len(f.Payload) >> 8), byte(len(f.Payload) & 0xFF)}
	if _, err := fw.w.Write(hdr); err != nil {
		return err
	}
	if len(f.Payload) > 0 {
		_, err := fw.w.Write(f.Payload)
		return err
	}
	return nil
}

// -----------------------------------------------------------------------------
// Utilities
// -----------------------------------------------------------------------------

func decodeConfig(p any) (Config, error) {
	var cfg Config
	switch v := p.(type) {
	case []byte:
		if err := json.Unmarshal(v, &cfg); err != nil {
			return cfg, err
		}
	case string:
		if err := json.Unmarshal([]byte(v), &cfg); err != nil {
			return cfg, err
		}
	case map[string]any:
		// Already a decoded object (e.g. if provided internally); re-marshal for simplicity.
		b, err := json.Marshal(v)
		if err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, err
		}
	default:
		return cfg, fmt.Errorf("unsupported config payload type: %T", p)
	}
	return cfg, nil
}

func (s *Service) publishState(level, status string, err error) {
	payload := map[string]any{
		"level":  level,  // "up", "degraded", "error", "idle"
		"status": status, // short machine string
		"ts_ms":  time.Now().UnixMilli(),
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	msg := s.conn.NewMessage(s.stateTopic, payload, true)
	s.conn.Publish(msg)
}

func backoffSeq(min, max time.Duration) func() time.Duration {
	if min <= 0 {
		min = 100 * time.Millisecond
	}
	if max < min {
		max = min
	}
	var cur = min
	return func() time.Duration {
		d := cur
		cur *= 2
		if cur > max {
			cur = max
		}
		return d
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

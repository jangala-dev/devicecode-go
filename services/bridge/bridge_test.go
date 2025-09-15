// bridge/bridge_test.go
package bridge

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"devicecode-go/bus"
)

func TestBridge_EstablishesUARTLinkAndReportsState(t *testing.T) {
	b := bus.NewBus(16)
	conn := b.NewConnection("bridge_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Start(ctx, conn)

	// Subscribe to bridge/state (retained) and verify initial status.
	stateSub := conn.Subscribe(bus.Topic{"bridge", "state"})
	defer conn.Unsubscribe(stateSub)

	first := nextStatePayload(t, stateSub, 500*time.Millisecond)
	assertLevelStatus(t, first, "idle", "awaiting_config")

	// Inject a UART dialler that returns a net.Pipe; keep the remote end to simulate link loss.
	prevDial := UARTDial
	defer func() { UARTDial = prevDial }()
	var remote io.ReadWriteCloser
	UARTDial = func(ctx context.Context, _ UARTConfig) (io.ReadWriteCloser, error) {
		lc, rc := net.Pipe()
		remote = rc
		// Remote peer loop: respond to ping frames; ignore others.
		go remotePeer(rc)
		return lc, nil
	}

	// Publish a valid UART config.
	cfg := `{"transport":{"type":"uart","uart":{"baud":115200,"rx_pin":1,"tx_pin":0}}}`
	conn.Publish(conn.NewMessage(bus.Topic{"config", "bridge"}, cfg, false))

	up := nextStatePayload(t, stateSub, time.Second)
	assertLevelStatus(t, up, "up", "link_established")

	// Close the remote to force link loss; expect degraded state.
	if remote != nil {
		_ = remote.Close()
	}

	degraded := nextStatePayload(t, stateSub, time.Second)
	assertLevelStatus(t, degraded, "degraded", "link_lost_retrying")
}

func TestBridge_UnknownTransportYieldsErrorState(t *testing.T) {
	b := bus.NewBus(8)
	conn := b.NewConnection("bridge_test_bad")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Start(ctx, conn)

	stateSub := conn.Subscribe(bus.Topic{"bridge", "state"})
	defer conn.Unsubscribe(stateSub)

	_ = nextStatePayload(t, stateSub, 500*time.Millisecond) // initial awaiting_config

	// Publish a config with an unknown transport type.
	cfg := `{"transport":{"type":"bogus"}}`
	conn.Publish(conn.NewMessage(bus.Topic{"config", "bridge"}, cfg, false))

	errState := nextStatePayload(t, stateSub, time.Second)
	assertLevelStatus(t, errState, "error", "transport_init_failed")
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// remotePeer minimally services the framing used by the bridge: it replies PONG to PING
// and drains any payload of other frames. It exits on read/write error.
func remotePeer(c io.ReadWriteCloser) {
	defer c.Close()
	hdr := make([]byte, 3)
	buf := make([]byte, 0, 256)
	for {
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		typ := hdr[0]
		n := int(hdr[1])<<8 | int(hdr[2])
		if n > 0 {
			if cap(buf) < n {
				buf = make([]byte, n)
			} else {
				buf = buf[:n]
			}
			if _, err := io.ReadFull(c, buf); err != nil {
				return
			}
		}
		// If we receive a ping (0x01), reply with pong (0x02).
		if typ == 0x01 {
			// type, length MSB, length LSB (no payload)
			if _, err := c.Write([]byte{0x02, 0x00, 0x00}); err != nil {
				return
			}
		}
	}
}

func nextStatePayload(t *testing.T, sub *bus.Subscription, d time.Duration) map[string]any {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case m := <-sub.Channel():
		p, ok := m.Payload.(map[string]any)
		if !ok {
			t.Fatalf("state payload type: got %T, want map[string]any", m.Payload)
		}
		return p
	case <-timer.C:
		t.Fatalf("timeout waiting for bridge/state")
		return nil
	}
}

func assertLevelStatus(t *testing.T, payload map[string]any, wantLevel, wantStatus string) {
	t.Helper()
	gotLevel, _ := payload["level"].(string)
	gotStatus, _ := payload["status"].(string)
	if gotLevel != wantLevel || gotStatus != wantStatus {
		t.Fatalf("unexpected state: level=%q status=%q, want level=%q status=%q (payload=%v)",
			gotLevel, gotStatus, wantLevel, wantStatus, payload)
	}
}

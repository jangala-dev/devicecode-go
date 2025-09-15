// config/config_test.go
package config

import (
	"context"
	"testing"
	"time"

	"devicecode-go/bus"
)

func TestConfig_PublishEmbedded_RetainedPerKey(t *testing.T) {
	// Override lookup for this test.
	oldLookup := EmbeddedConfigLookup
	EmbeddedConfigLookup = func(device string) ([]byte, bool) {
		if device != "pico" {
			return nil, false
		}
		return []byte(`{
			"mode": "dev",
			"debug": true,
			"region": {"code": "eu"}
		}`), true
	}
	t.Cleanup(func() { EmbeddedConfigLookup = oldLookup })

	// Arrange bus and service.
	b := bus.NewBus(16)
	conn := b.NewConnection("test-config")
	svc := NewConfigService()

	// Start publisher with device ID in context.
	ctx := context.WithValue(context.Background(), ctxDeviceKey, "pico")
	svc.Start(ctx, conn)

	// Subscribe; retained messages should arrive immediately.
	sub := conn.Subscribe(bus.Topic{configPrefix, "#"})

	type gotMsg struct {
		key string
		val any
	}

	wantCount := 3 // mode, debug, region
	got := map[string]gotMsg{}

	deadline := time.Now().Add(600 * time.Millisecond)
	for len(got) < wantCount && time.Now().Before(deadline) {
		select {
		case m := <-sub.Channel():
			if len(m.Topic) < 2 {
				t.Fatalf("unexpected topic length: %#v", m.Topic)
			}
			// Assert tokens to string
			prefix, ok := m.Topic[0].(string)
			if !ok {
				t.Fatalf("topic[0] type %T, want string", m.Topic[0])
			}
			if prefix != configPrefix {
				t.Fatalf("unexpected prefix: %q", prefix)
			}
			keyTok := m.Topic[1]
			key, ok := keyTok.(string)
			if !ok {
				t.Fatalf("topic[1] type %T, want string", keyTok)
			}
			got[key] = gotMsg{key: key, val: m.Payload}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if len(got) != wantCount {
		t.Fatalf("expected %d retained messages, got %d (%v)", wantCount, len(got), got)
	}

	// Assert payloads without reflect.
	// mode
	if v, ok := got["mode"]; !ok {
		t.Fatal("missing 'mode' message")
	} else if s, ok := v.val.(string); !ok || s != "dev" {
		t.Fatalf("mode payload = %#v, want \"dev\"", v.val)
	}
	// debug
	if v, ok := got["debug"]; !ok {
		t.Fatal("missing 'debug' message")
	} else if bval, ok := v.val.(bool); !ok || bval != true {
		t.Fatalf("debug payload = %#v, want true", v.val)
	}
	// region
	if v, ok := got["region"]; !ok {
		t.Fatal("missing 'region' message")
	} else if m, ok := v.val.(map[string]any); !ok {
		t.Fatalf("region payload type = %T, want map[string]any", v.val)
	} else if code, ok := m["code"].(string); !ok || code != "eu" {
		t.Fatalf("region.code = %#v, want \"eu\"", m["code"])
	}
}

func TestConfig_PublishConfig_MissingDevice(t *testing.T) {
	b := bus.NewBus(4)
	conn := b.NewConnection("test-missing-device")
	svc := NewConfigService()

	// No device ID in context
	if err := svc.publishConfig(context.Background(), conn); err == nil {
		t.Fatal("expected error for missing device ID, got nil")
	}
}

func TestConfig_PublishConfig_NoConfigFound(t *testing.T) {
	// Override lookup to simulate absence.
	oldLookup := EmbeddedConfigLookup
	EmbeddedConfigLookup = func(device string) ([]byte, bool) { return nil, false }
	t.Cleanup(func() { EmbeddedConfigLookup = oldLookup })

	b := bus.NewBus(4)
	conn := b.NewConnection("test-no-config")
	svc := NewConfigService()

	ctx := context.WithValue(context.Background(), ctxDeviceKey, "unknown-device")
	if err := svc.publishConfig(ctx, conn); err == nil {
		t.Fatal("expected error for missing embedded config, got nil")
	}
}

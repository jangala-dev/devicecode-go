package otadiag

import (
	"strings"
	"testing"
)

func TestDefaultFilterSuppressesHighVolumeEvents(t *testing.T) {
	lines, restore := captureDefaultFilteredEvents()
	defer restore()

	Event("[mcu-ota]", "heartbeat", "xfer-1", KV("state", "receiving"))
	Event("[fabric-xfer]", "chunk_rx", "xfer-1", KV("offset", 0), KV("expected", 0))
	Event("[fabric-xfer]", "chunk_decode_done", "xfer-1", KV("ok", true), KV("raw_len", 512))
	Event("[fabric-xfer]", "chunk_digest_done", "xfer-1", KV("ok", true))
	Event("[fabric-xfer]", "sink_write_start", "xfer-1", KV("offset", 0))
	Event("[fabric-xfer]", "sink_write_done", "xfer-1", KV("next", 512))
	Event("[fabric-xfer]", "gc_done", "xfer-1", KV("next", 512))
	Event("[updater-stream]", "stream_write_start", "xfer-1", KV("len", 512))
	Event("[updater-stream]", "image_signature_verify_start", "xfer-1", KV("manifest_len", 192))
	Event("[updater-stream]", "flash_program_start", "xfer-1", KV("offset", 0))

	if got := strings.Join(*lines, "\n"); got != "" {
		t.Fatalf("default filter emitted high-volume diagnostics:\n%s", got)
	}
}

func TestDefaultFilterKeepsActionableEvents(t *testing.T) {
	lines, restore := captureDefaultFilteredEvents()
	defer restore()

	Event("[serial-raw]", "rx_driver_pressure", XferNone, KV("uart", "uart0"))
	Event("[fabric-rx]", "read_line", XferNone, KV("type", "ping"))
	Event("[fabric-rpc]", "sent", XferNone, KV("call_id", "call-1"))
	Event("[mcu-ota]", "heartbeat_start", "xfer-1", KV("reason", "prepare"))
	Event("[mcu-ota]", "heartbeat_stop", "xfer-1", KV("reason", "done"))
	Event("[fabric-xfer]", "begin_rx", "xfer-1", KV("target", "updater/main"))
	Event("[fabric-xfer]", "ready_tx", "xfer-1", KV("ok", true))
	Event("[fabric-xfer]", "need_tx", "xfer-1", KV("next", 0), KV("ok", true))
	Event("[fabric-xfer]", "chunk_digest_done", "xfer-1", KV("ok", false), KV("reason", "chunk_digest_mismatch"))
	Event("[fabric-xfer]", "sink_write_error", "xfer-1", KV("reason", "write_boom"))
	Event("[updater-stream]", "prepare_rx", XferNone, KV("job_id", "job-1"))
	Event("[updater-stream]", "flash_erase_start", "xfer-1", KV("offset", 0))
	Event("[updater-stream]", "image_signature_verify_error", "xfer-1", KV("reason", "bad_signature"))

	got := strings.Join(*lines, "\n")
	for _, want := range []string{
		"[serial-raw]", "[fabric-rx]", "[fabric-rpc]",
		"ev heartbeat_start", "ev heartbeat_stop",
		"ev begin_rx", "ev ready_tx", "ev need_tx",
		"ev chunk_digest_done", "ev sink_write_error",
		"ev prepare_rx", "ev flash_erase_start", "ev image_signature_verify_error",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("default filter output missing %q:\n%s", want, got)
		}
	}
}

func TestSinkForTestEnablesVerboseDiagnostics(t *testing.T) {
	var lines []string
	restore := SetSinkForTest(func(line string) {
		lines = append(lines, line)
	})
	defer restore()

	Event("[fabric-xfer]", "chunk_rx", "xfer-1", KV("offset", 0), KV("expected", 0))
	Event("[updater-stream]", "image_signature_verify_start", "xfer-1", KV("manifest_len", 192))

	got := strings.Join(lines, "\n")
	for _, want := range []string{"ev chunk_rx", "ev image_signature_verify_start"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose test sink output missing %q:\n%s", want, got)
		}
	}
}

func captureDefaultFilteredEvents() (*[]string, func()) {
	lines := []string{}
	sinkMu.Lock()
	prevSink := sink
	prevVerbose := verbose.Load()
	sink = func(line string) {
		lines = append(lines, line)
	}
	verbose.Store(false)
	sinkMu.Unlock()
	return &lines, func() {
		sinkMu.Lock()
		sink = prevSink
		verbose.Store(prevVerbose)
		sinkMu.Unlock()
	}
}

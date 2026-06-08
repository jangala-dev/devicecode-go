package fabric

import (
	"context"
	"sync/atomic"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/updater"
	"devicecode-go/x/strconvx"
)

// Transport abstracts the byte stream as newline-delimited JSON lines.
type Transport interface {
	ReadLine() ([]byte, error)
	WriteLine(data []byte) error
	Close() error
}

const defaultLinkID = "mcu-uart0"

// LinkConfig carries the fabric link parameters that the CM5 publishes
// alongside its own session/transfer-mgr instances. Mirrors the relevant
// keys in `bigbox-v1-cm-2.json` `fabric.data.links.<id>` for the
// MCU-facing link. Missing fields fall back to release defaults via
// applyDefaults so callers can pass `LinkConfig{}` to mean "release".
type LinkConfig struct {
	// ChunkSize is the expected raw-byte payload per xfer_chunk. The MCU
	// is receive-only for transfers, so this is informational/validation
	// only on the Go side. Release: 2048 bytes.
	ChunkSize uint32
	// PhaseTimeout is the idle-chunk watchdog: an active inbound transfer
	// is aborted with reason="timeout" if no xfer_chunk arrives within
	// this window. Mirrors transfer_mgr.lua's `phase_timeout`.
	// Release: 15s.
	PhaseTimeout time.Duration
	// PingInterval drives the unconditional outbound ping cadence after
	// the link is established (`session_ctl.lua` resets next_ping_at =
	// now + ping_interval after every send; not TX-activity-based).
	// Release: 10s.
	PingInterval time.Duration
	// LivenessTimeout tears the link down if no frame arrives within
	// this window once established. Mirrors session_ctl.lua's
	// liveness_timeout_s. Release: 30s.
	LivenessTimeout time.Duration
	// TargetCallTimeout is the local updater/main stage RPC deadline after
	// xfer_commit has verified the wire transfer. The Fabric session owns this
	// as pending operation state; it must not block the reactor loop.
	// Release: 5s.
	TargetCallTimeout time.Duration
	// MaxInboundHelpers caps the number of in-flight inbound RPC calls.
	// Excess inbound calls reply `{ok=false, err="busy"}` per
	// rpc_bridge.lua's `spawn_local_call_helper`. Lua default is 64
	// (falls back to max_pending_calls); we keep that for parity.
	MaxInboundHelpers int
	// RPCQuantum and BulkQuantum control the writer's weighted
	// round-robin between the rpc and bulk lanes after the control
	// lane drains. Mirrors writer.lua's lane scheduler. Release: 4 and 1.
	RPCQuantum  int
	BulkQuantum int
}

func DefaultLinkConfig() LinkConfig {
	return LinkConfig{
		ChunkSize:         2048,
		PhaseTimeout:      15 * time.Second,
		PingInterval:      10 * time.Second,
		LivenessTimeout:   30 * time.Second,
		TargetCallTimeout: 5 * time.Second,
		MaxInboundHelpers: 64,
		RPCQuantum:        4,
		BulkQuantum:       1,
	}
}

func (c *LinkConfig) applyDefaults() {
	d := DefaultLinkConfig()
	if c.ChunkSize == 0 {
		c.ChunkSize = d.ChunkSize
	}
	if c.PhaseTimeout == 0 {
		c.PhaseTimeout = d.PhaseTimeout
	}
	if c.PingInterval == 0 {
		c.PingInterval = d.PingInterval
	}
	if c.LivenessTimeout == 0 {
		c.LivenessTimeout = d.LivenessTimeout
	}
	if c.TargetCallTimeout == 0 {
		c.TargetCallTimeout = d.TargetCallTimeout
	}
	if c.MaxInboundHelpers == 0 {
		c.MaxInboundHelpers = d.MaxInboundHelpers
	}
	if c.RPCQuantum == 0 {
		c.RPCQuantum = d.RPCQuantum
	}
	if c.BulkQuantum == 0 {
		c.BulkQuantum = d.BulkQuantum
	}
}

var nextSessionID atomic.Uint64

func newLocalSID() string {
	bootID := updater.BootID()
	if bootID == "" {
		bootID = updater.GenerateBootID()
	}
	return "mcu-sid-" + bootID + "-" + strconvx.Utoa64(nextSessionID.Add(1))
}

// Run starts the fabric session. Blocks until ctx is cancelled or the
// transport returns an unrecoverable error. The MCU is a hello
// responder (CM5 always initiates hello/hello_ack), but otherwise
// runs the symmetric session_ctl semantics: once established, it
// sends pings every PingInterval and tears the link down if no frame
// arrives within LivenessTimeout. Mirrors session_ctl.lua at
// devicecode-lua@2c88090.
func Run(ctx context.Context, tr Transport, conn *bus.Connection, nodeID, peerID string, cfg LinkConfig) {
	s := session{
		linkID:   defaultLinkID,
		nodeID:   nodeID,
		peerID:   peerID,
		localSID: newLocalSID(),
		tr:       tr,
		conn:     conn,
		cfg:      cfg,
	}
	s.run(ctx)
}

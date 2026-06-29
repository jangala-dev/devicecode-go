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
	// MaxAcceptedChunkSize is the receive-side upper bound for the raw-byte
	// payload in one xfer_chunk. The sender owns the actual chunk size; the MCU
	// must accept at least 2048 bytes for fabric-jsonl/1 v1. Release: 2048 bytes.
	MaxAcceptedChunkSize uint32
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
		MaxAcceptedChunkSize: MaxAcceptedChunkSize,
		PhaseTimeout:         15 * time.Second,
		PingInterval:         10 * time.Second,
		LivenessTimeout:      30 * time.Second,
		TargetCallTimeout:    5 * time.Second,
		MaxInboundHelpers:    64,
		RPCQuantum:           4,
		BulkQuantum:          1,
	}
}

func (c *LinkConfig) applyDefaults() {
	d := DefaultLinkConfig()
	if c.MaxAcceptedChunkSize == 0 {
		c.MaxAcceptedChunkSize = d.MaxAcceptedChunkSize
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

// StageController is Fabric's narrow boundary to an updater/main staging
// owner. Fabric submits transfer bytes and observes command results; it does
// not own updater state or flash/verifier work.
type StageController interface {
	BeginStreamedStage(xferID string, size uint32) (uint64, error)
	WriteStreamedStage(xferID string, generation uint64, data []byte) error
	CommitStreamedStage(xferID string, generation uint64) (uint32, error)
	AbortStreamedStage(xferID string, generation uint64, reason string)
	CancelStreamedStage(xferID string, generation uint64, reason string)
}

// RunOptions carries optional dependencies that do not belong in the wire
// LinkConfig. Keeping the updater staging controller here makes the local
// Fabric-to-Updater boundary explicit; Fabric no longer locates the updater
// service through package-global state.
type RunOptions struct {
	Buffers         *FabricBuffers
	StageController StageController
}

// Run starts the fabric session. Blocks until ctx is cancelled or the
// transport returns an unrecoverable error. The MCU is a hello
// responder (CM5 always initiates hello/hello_ack), but otherwise
// runs the symmetric session_ctl semantics: once established, it
// sends pings every PingInterval and tears the link down if no frame
// arrives within LivenessTimeout. Mirrors session_ctl.lua at
// devicecode-lua@2c88090.
func Run(ctx context.Context, tr Transport, conn *bus.Connection, nodeID, peerID string, cfg LinkConfig) {
	RunWithBuffers(ctx, tr, conn, nodeID, peerID, cfg, nil)
}

func RunWithBuffers(ctx context.Context, tr Transport, conn *bus.Connection, nodeID, peerID string, cfg LinkConfig, buffers *FabricBuffers) {
	RunWithOptions(ctx, tr, conn, nodeID, peerID, cfg, RunOptions{Buffers: buffers})
}

func RunWithOptions(ctx context.Context, tr Transport, conn *bus.Connection, nodeID, peerID string, cfg LinkConfig, opts RunOptions) {
	s := session{
		linkID:          defaultLinkID,
		nodeID:          nodeID,
		peerID:          peerID,
		localSID:        newLocalSID(),
		tr:              tr,
		conn:            conn,
		cfg:             cfg,
		stageController: opts.StageController,
		buffers:         ensureFabricBuffers(opts.Buffers),
	}
	s.run(ctx)
}

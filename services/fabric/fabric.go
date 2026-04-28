package fabric

import (
	"context"
	"sync/atomic"
	"time"

	"devicecode-go/bus"
	"devicecode-go/x/strconvx"
)

// Transport abstracts the byte stream as newline-delimited JSON lines.
type Transport interface {
	ReadLine() ([]byte, error)
	WriteLine(data []byte) error
	Close() error
}

const protoVersion = 1
const defaultLinkID = "mcu0"

// LinkConfig carries the fabric link parameters that the CM5 publishes
// alongside its own session/transfer-mgr instances. Mirrors the relevant
// keys in `bigbox-v1-cm-2.json` `service.fabric.links.<id>` for the
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
}

func DefaultLinkConfig() LinkConfig {
	return LinkConfig{
		ChunkSize:    2048,
		PhaseTimeout: 15 * time.Second,
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
}

var nextSessionID atomic.Uint64

func newLocalSID() string {
	return "mcu-sid-" + strconvx.Utoa64(nextSessionID.Add(1))
}

// Run starts the fabric session. Blocks until ctx is cancelled or the
// transport returns an unrecoverable error. The MCU is respond-only:
// it never initiates hello or ping. It waits for hello from the CM5
// and replies with hello_ack; it responds to ping with pong. The CM5
// owns heartbeat cadence — the MCU marks the link stale if nothing
// arrives within the timeout.
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

package fabric

import (
	"context"
	"sync/atomic"

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
func Run(ctx context.Context, tr Transport, conn *bus.Connection, nodeID, peerID string) {
	s := session{
		linkID:          defaultLinkID,
		nodeID:          nodeID,
		peerID:          peerID,
		localSID:        newLocalSID(),
		tr:   tr,
		conn: conn,
	}
	s.run(ctx)
}

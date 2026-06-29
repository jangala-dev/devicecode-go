package fabric

import (
	"context"
	"errors"

	"devicecode-go/bus"
)

// ErrRemoteCallsStubbed marks the deliberate placeholder for future MCU -> CM5
// dependency ports. The old generic outbound remap bridge has been removed; new
// remote operations should be introduced as narrow typed ports owned by the
// service that needs them.
var ErrRemoteCallsStubbed = errors.New("fabric: remote capability calls are stubbed")

// CM5TimeSnapshotWireTopic is the intended future endpoint for MCU wall-clock
// synchronisation. It is declared here so time sync can be added without
// reintroducing generic outbound call routing.
var CM5TimeSnapshotWireTopic = []string{"cap", "peer", "cm5", "time", "main", "rpc", "snapshot"}

// RemoteClient is a narrow placeholder for future declared dependency ports,
// such as an MCU time service asking CM5 for a Unix-time snapshot.
//
// It intentionally does not route anything today. When a concrete feature is
// added, implement the specific port on top of Fabric rather than restoring
// the old bus-topic exportCall remapping machinery.
type RemoteClient struct {
	conn *bus.Connection
}

func NewRemoteClient(conn *bus.Connection) RemoteClient {
	return RemoteClient{conn: conn}
}

func (c RemoteClient) Call(ctx context.Context, wireTopic []string, payload any) (any, error) {
	_ = c.conn
	_ = ctx
	_ = wireTopic
	_ = payload
	return nil, ErrRemoteCallsStubbed
}

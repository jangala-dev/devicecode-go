package heartbeat

import (
	"context"
	"time"

	"devicecode-go/bus"
)

var topicConfigHeartbeat = bus.Topic{"config", "heartbeat"}

type Service struct {}

func (s *Service) serviceLoop(ctx context.Context, conn *bus.Connection) {
	cfgSub := conn.Subscribe(topicConfigHeartbeat)
	defer conn.Unsubscribe(cfgSub)

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	// loop until context is cancelled, respond to tick and config changes
	for {
		select {
		case <-ctx.Done():
			println("Info: heartbeat service stopping")
			return
		case t := <-tick.C:
			println("Info:", t.Format("15:04:05"), "Heartbeat")
		case msg := <-cfgSub.Channel():
			println("Info:", "Received config message:", msg.Payload)			
			// Change tick interval if needed
			if m, ok := msg.Payload.(map[string]any); ok {
				if iv, ok := m["interval"]; ok {
					if interval, ok := iv.(float64); ok {
						tick.Reset(time.Duration(interval) * time.Second)
						println("Info:", "Heartbeat interval set to", interval, "seconds")
					}
				}
			}
		}
	}
}

// Start the heartbeat service.
func (s *Service) Start(ctx context.Context, conn *bus.Connection) error {
	go s.serviceLoop(ctx, conn)
	return nil
}
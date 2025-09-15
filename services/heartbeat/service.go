package heartbeat

import (
	"context"
	"time"
)

type Service struct {}

func (s *Service) serviceLoop(ctx context.Context) {
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for t := range tick.C {
		select {
		case <-ctx.Done():
			return
		default:
		}

		println("Info:", t.Format("15:04:05"), "Heartbeat")
	}
}

// Start the heartbeat service.
func (s *Service) Start(ctx context.Context) error {
	go s.serviceLoop(ctx)
	return nil
}
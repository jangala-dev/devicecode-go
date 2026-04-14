package main

import (
	"context"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/services/reactor"
	"devicecode-go/types"
	"devicecode-go/utilities"
)

// HAL
const halTimeout = 5 * time.Second
var halReadiness = bus.T("hal", "state")

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	// Allow early USB/console settle if needed
	time.Sleep(3 * time.Second)
	log.SetStart(time.Now())

	ctx := context.Background()

	log.Println("[main] bootstrapping bus …")
	// Queue length must cover the retained replay burst when fabric
	// subscribes to wildcard export patterns (hal/cap/env/#,
	// hal/cap/power/#). Each capability publishes retained info +
	// status + value; pico_bb_proto_1 has ~26 retained topics across
	// env and power domains. 32 provides margin for growth.
	b := bus.NewBus(32, "+", "#")
	halConn := b.NewConnection("hal")
	uiConn := b.NewConnection("ui")

	log.Println("[main] starting hal.Run …")
	go hal.Run(ctx, halConn)

	// Wait for retained hal/state=ready (or time out)
	if !waitHALReady(ctx, halConn, halTimeout) {
		for {
			log.Println("[main] HAL not ready within timeout")
			time.Sleep(2 * time.Second)
		}
	}

	// Reactor
	r := reactor.NewReactor(b, uiConn)
	r.Run(ctx)
}

// -----------------------------------------------------------------------------
// HAL readiness helper
// -----------------------------------------------------------------------------

func waitHALReady(ctx context.Context, c *bus.Connection, d time.Duration) bool {
	sub := c.Subscribe(halReadiness)
	defer c.Unsubscribe(sub)

	ctx2, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	for {
		select {
		case m := <-sub.Channel():
			if st, ok := m.Payload.(types.HALState); ok && st.Level == "ready" {
				return true
			}
		case <-ctx2.Done():
			return false
		}
	}
}

// Global logger instance
var log = utilities.Logger{LineStart: true}
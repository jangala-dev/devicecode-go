package main

import (
	"context"
	"time"
	"devicecode-go/services/heartbeat"
)

var services = [1]string{"heartbeat"}

func main() {
	// Allow USB CDC to enumerate before we print.
	time.Sleep(2 * time.Second)
	println("boot")

	// Start services.
	ctx := context.Background()
	for _, svc := range services {
		switch svc {
		case "heartbeat":
			hb := &heartbeat.Service{}
			if err := hb.Start(ctx); err != nil {
				println("Error:", "heartbeat service error:", err.Error())
			}
		}
	}

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for t := range tick.C {
		println("Info:", t.Format("15:04:05"), "Main loop")
	}
}

package main

import (
	"context"
	"time"
	"devicecode-go/bus"
	"devicecode-go/services/config"
	"devicecode-go/services/heartbeat"
)

var services = []string{"config", "heartbeat"}

func main() {
	// Allow USB CDC to enumerate before we print.
	time.Sleep(2 * time.Second)
	println("boot")

	// Create a context that can be cancelled, with a value for device ID.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, config.CtxDeviceKey, "pico")

	bus := bus.NewBus(100)
	conn := bus.NewConnection("main")
	
	for _, svc := range services {
		switch svc {
		case "heartbeat":
			hb := &heartbeat.Service{}
			if err := hb.Start(ctx, conn); err != nil {
				println("Error:", "heartbeat service error:", err.Error())
			}
		case "config":
			cfg := config.NewConfigService()
			cfg.Start(ctx, conn)
		}
	}


	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for t := range tick.C {
		println("Info:", t.Format("15:04:05"), "Main loop")
	}
}

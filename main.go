package main

import (
	"time"
)

func main() {
	// Allow USB CDC to enumerate before we print.
	time.Sleep(2 * time.Second)
	println("boot")

	// Periodic stats.
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for t := range tick.C {
		println(t.Format("15:04:05"), "Heartbeat")
	}
}

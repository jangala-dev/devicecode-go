package main

import (
	"context"
	"runtime"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/fabric"
	"devicecode-go/services/updater"
)

const (
	payloadSize = 1024
	chunkSize   = 256
)

func main() {
	// Give the USB/monitor path a short settle window, matching the appliance
	// firmware's behaviour.  This image is intentionally not the appliance: it is
	// a narrow board-level Fabric protocol gate.
	time.Sleep(3 * time.Second)
	println("0.000 [fabric-selftest-fw] bootstrapping bus")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := bus.NewBus(3, "+", "#")
	mainConn := b.NewConnection("fabric-selftest-main")
	updaterConn := b.NewConnection("updater")
	fabricConn := b.NewConnection("fabric-selftest")

	readySub := mainConn.Subscribe(updater.TopicUpdaterFact)
	defer mainConn.Unsubscribe(readySub)

	updater.GenerateBootID()
	updaterSvc := updater.New(updater.Options{
		Conn: updaterConn,
		Identity: updater.Identity{
			Version: "fabric-selftest",
			Build:   "standalone",
			ImageID: "fabric-selftest-image",
		},
	})
	go updaterSvc.Run(ctx)
	println("0.000 [fabric-selftest-fw] updater started")

	if !waitUpdaterReady(ctx, readySub, 2*time.Second) {
		println("0.000 [fabric-selftest-fw] updater not ready")
		for {
			time.Sleep(2 * time.Second)
		}
	}

	println("0.000 [fabric-selftest-fw] starting fabric transfer self-test")
	res, err := fabric.RunUARTSelfTest(ctx, fabric.UARTSelfTestOptions{
		Conn:            fabricConn,
		StageController: updaterSvc,
		PayloadSize:     payloadSize,
		ChunkSize:       chunkSize,
		Timeout:         10 * time.Second,
	})
	if err != nil {
		println("0.000 [fabric-selftest-fw] failed", err.Error())
	} else {
		println("0.000 [fabric-selftest-fw] ok xfer=", res.XferID, "bytes=", int(res.PayloadSize), "chunk=", int(res.ChunkSize), "digest=", res.Digest)
	}

	// Stop active service goroutines after the test. Keep the image alive so the
	// monitor remains connected and the heap profile can be observed.
	cancel()
	for {
		printMem()
		time.Sleep(3 * time.Second)
	}
}

func waitUpdaterReady(ctx context.Context, sub *bus.Subscription, d time.Duration) bool {
	ctx2, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for {
		select {
		case m := <-sub.Channel():
			if m != nil {
				return true
			}
		case <-ctx2.Done():
			return false
		}
	}
}

func printMem() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	println("0.000 [fabric-selftest-fw] mem alloc:", int(m.Alloc), "heapSys:", int(m.HeapSys), "mallocs:", int(m.Mallocs), "frees:", int(m.Frees))
}

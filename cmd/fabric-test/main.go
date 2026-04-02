//go:build tinygo && rp2350

// fabric-test: exercises the fabric protocol over USB serial with real HAL sensors.
//
//   tinygo build -target=pico2 -tags "pico_bb_proto_1" -stack-size=8KB -o build/fabric-test.elf ./cmd/fabric-test

package main

import (
	"context"
	"machine"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/fabric"
	"devicecode-go/services/hal"
	"devicecode-go/types"
)

const halTimeout = 5 * time.Second

var halReadiness = bus.T("hal", "state")

func main() {
	time.Sleep(3 * time.Second)

	ctx := context.Background()
	b := bus.NewBus(3, "+", "#")
	halConn := b.NewConnection("hal")

	go hal.Run(ctx, halConn)
	if !waitHALReady(ctx, halConn, halTimeout) {
		return
	}

	conn := b.NewConnection("fabric")
	tr := fabric.NewRWTransport(&serialRW{}, &serialRW{})
	fabric.Run(ctx, tr, conn, "mcu-1", "cm5-local")
}

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

type serialRW struct{}

func (s *serialRW) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for machine.Serial.Buffered() == 0 {
		time.Sleep(time.Millisecond)
	}
	n := 0
	for n < len(p) && machine.Serial.Buffered() > 0 {
		b, err := machine.Serial.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		p[n] = b
		n++
	}
	return n, nil
}

func (s *serialRW) Write(p []byte) (int, error) {
	return machine.Serial.Write(p)
}

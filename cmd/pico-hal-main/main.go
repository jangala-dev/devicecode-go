package main

import (
	"context"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"
)

// tiny helpers (no fmt)
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	sign := ""
	if i < 0 {
		sign = "-"
		i = -i
	}
	var buf [32]byte
	b := len(buf)
	for i > 0 {
		b--
		buf[b] = byte('0' + (i % 10))
		i /= 10
	}
	if sign != "" {
		b--
		buf[b] = '-'
	}
	return string(buf[b:])
}
func topicStr(t bus.Topic) string {
	n := t.Len()
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += "/"
		}
		switch v := t.At(i).(type) {
		case string:
			out += v
		case int:
			out += itoa(v)
		case int32:
			out += itoa(int(v))
		case int64:
			out += itoa(int(v))
		case float64:
			out += itoa(int(v))
		default:
			out += "?"
		}
	}
	return out
}

func main() {
	time.Sleep(3 * time.Second)
	ctx := context.Background()

	println("[main] bootstrapping bus …")
	b := bus.NewBus(4)
	halConn := b.NewConnection("hal")
	uiConn := b.NewConnection("ui")

	println("[main] subscribing to hal/# for diagnostics …")
	mon := uiConn.Subscribe(bus.T("hal", "#"))
	go func() {
		for m := range mon.Channel() {
			println("[monitor] <-", topicStr(m.Topic))
		}
	}()

	println("[main] starting hal.Run …")
	go hal.Run(ctx, halConn)

	// Publish a public, strongly-typed HALConfig
	cfg := types.HALConfig{
		Devices: []types.HALDevice{
			{
				ID:   "led0",
				Type: "gpio_led",
				Params: types.LEDParams{
					Pin:     25,
					Initial: false,
				},
			},
		},
	}
	println("[main] publishing config/hal …")
	uiConn.Publish(uiConn.NewMessage(bus.T("config", "hal"), cfg, true))

	time.Sleep(250 * time.Millisecond)

	// Try read_now on capability led/0
	readNow := bus.T("hal", "capability", string(types.KindLED), 0, "control", "read_now")
	println("[main] sending read_now for led/0 …")
	if reply, err := uiConn.RequestWait(ctx, uiConn.NewMessage(readNow, map[string]any{}, false)); err != nil {
		println("[main] read_now error:", err.Error())
	} else {
		println("[main] read_now reply on", topicStr(reply.Topic))
	}

	toggle := bus.T("hal", "capability", string(types.KindLED), 0, "control", "toggle")

	for {
		if _, err := uiConn.RequestWait(ctx, uiConn.NewMessage(toggle, map[string]any{}, false)); err != nil {
			println("[main] toggle error:", err.Error())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

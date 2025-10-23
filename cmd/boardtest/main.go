// cmd/boardtest/main.go
package main

import (
	"context"
	"fmt"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"
	"devicecode-go/x/shmring"
)

// ---------- Configuratin ----------

const (
	halReadyTimeout = 5 * time.Second

	// Sequencing timing
	stepDelayUp   = 300 * time.Millisecond
	stepDelayDown = 300 * time.Millisecond
	dwellUp       = 2 * time.Second
	dwellDown     = 2 * time.Second

	// Freshness
	freshMaxAge = 2 * time.Second

	// Cycles: 0 = loop forever
	cyclesToRun = 0
)

// Rails present in our Pico setups
var powerSeq = []string{
	"mpcie-usb",
	"m2",
	"mpcie",
	"cm5",
	"fan",
	"boost-load",
}

// ---------- Topics ----------

func tLEDSet() bus.Topic {
	return bus.T("hal", "cap", "io", string(types.KindLED), "button_led", "control", "set")
}
func tSwitch(name string) bus.Topic {
	return bus.T("hal", "cap", "power", string(types.KindSwitch), name, "control", "set")
}
func tHalState() bus.Topic { return bus.T("hal", "state") }

func tSessOpen(name string) bus.Topic {
	return bus.T("hal", "cap", "io", "serial", name, "control", "session_open")
}
func tSessOpened(name string) bus.Topic {
	return bus.T("hal", "cap", "io", "serial", name, "event", "session_opened")
}

var (
	tBattVal = bus.T("hal", "cap", "power", string(types.KindBattery), "internal", "value")
	tChgVal  = bus.T("hal", "cap", "power", string(types.KindCharger), "internal", "value")
	tTempVal = bus.T("hal", "cap", "env", string(types.KindTemperature), "core", "value")
	tHumVal  = bus.T("hal", "cap", "env", string(types.KindHumidity), "core", "value")
)

// ---------- Minimal output to console + both UARTS ----------

type out struct {
	u0, u1 *shmring.Ring
}

func (o *out) println(a ...any) {
	line := fmt.Sprintln(a...)
	print(line)
	if o.u0 != nil {
		_ = o.u0.TryWriteFrom([]byte(line))
	}
	if o.u1 != nil {
		_ = o.u1.TryWriteFrom([]byte(line))
	}
}

func (o *out) printf(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	print(line)
	if o.u0 != nil {
		_ = o.u0.TryWriteFrom([]byte(line))
	}
	if o.u1 != nil {
		_ = o.u1.TryWriteFrom([]byte(line))
	}
}

// ---------- Helpers ----------

func waitHALReady(c *bus.Connection, d time.Duration) bool {
	sub := c.Subscribe(tHalState())
	defer c.Unsubscribe(sub)

	dead := time.Now().Add(d)
	for time.Now().Before(dead) {
		select {
		case m := <-sub.Channel():
			if st, ok := m.Payload.(types.HALState); ok && st.Level == "ready" {
				return true
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	return false
}

func openUARTSessions(ui *bus.Connection, o *out) {
	sub0 := ui.Subscribe(tSessOpened("uart0"))
	sub1 := ui.Subscribe(tSessOpened("uart1"))
	defer ui.Unsubscribe(sub0)
	defer ui.Unsubscribe(sub1)

	// Fire open requests
	ui.Publish(ui.NewMessage(tSessOpen("uart0"), nil, false))
	ui.Publish(ui.NewMessage(tSessOpen("uart1"), nil, false))

	dead := time.Now().Add(3 * time.Second)
	for time.Now().Before(dead) && (o.u0 == nil || o.u1 == nil) {
		select {
		case m := <-sub0.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				o.u0 = shmring.Get(shmring.Handle(ev.TXHandle))
				o.println("[uart0] session opened")
			}
		case m := <-sub1.Channel():
			if ev, ok := m.Payload.(types.SerialSessionOpened); ok {
				o.u1 = shmring.Get(shmring.Handle(ev.TXHandle))
				o.println("[uart1] session opened")
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func ledFlashPassFail(ui *bus.Connection, pass bool) {
	if pass {
		// Double short
		for i := 0; i < 2; i++ {
			ui.Publish(ui.NewMessage(tLEDSet(), types.LEDSet{On: true}, false))
			time.Sleep(120 * time.Millisecond)
			ui.Publish(ui.NewMessage(tLEDSet(), types.LEDSet{On: false}, false))
			time.Sleep(200 * time.Millisecond)
		}
	} else {
		// Single long
		ui.Publish(ui.NewMessage(tLEDSet(), types.LEDSet{On: true}, false))
		time.Sleep(400 * time.Millisecond)
		ui.Publish(ui.NewMessage(tLEDSet(), types.LEDSet{On: false}, false))
		time.Sleep(200 * time.Millisecond)
	}
}

func setRail(ui *bus.Connection, name string, on bool) {
	ui.Publish(ui.NewMessage(tSwitch(name), types.SwitchSet{On: on}, false))
}

// ---------- Main ----------

func main() {
	ctx := context.Background()

	// Local bus and connections
	b := bus.NewBus(4, "+", "#")
	halConn := b.NewConnection("hal")
	ui := b.NewConnection("ui")

	// Start HAL
	go hal.Run(ctx, halConn)

	// Wait for HAL ready (non-fatal if slow)
	if !waitHALReady(halConn, halReadyTimeout) {
		println("[boardtest] HAL not ready within timeout; continuing")
	}

	// UART outputs
	var o out
	openUARTSessions(ui, &o)

	// Subscriptions for auto-polling values
	subBatt := ui.Subscribe(tBattVal)
	subChg := ui.Subscribe(tChgVal)
	subTmp := ui.Subscribe(tTempVal)
	subHum := ui.Subscribe(tHumVal)
	defer ui.Unsubscribe(subBatt)
	defer ui.Unsubscribe(subChg)
	defer ui.Unsubscribe(subTmp)
	defer ui.Unsubscribe(subHum)

	// Last-seen timestamps
	var tsBatt, tsChg, tsTmp, tsHum time.Time

	// Feed goroutine to track arrivals
	go func() {
		for {
			select {
			case m := <-subBatt.Channel():
				if _, ok := m.Payload.(types.BatteryValue); ok {
					tsBatt = time.Now()
				}
			case m := <-subChg.Channel():
				if _, ok := m.Payload.(types.ChargerValue); ok {
					tsChg = time.Now()
				}
			case m := <-subTmp.Channel():
				if _, ok := m.Payload.(types.TemperatureValue); ok {
					tsTmp = time.Now()
				}
			case m := <-subHum.Channel():
				if _, ok := m.Payload.(types.HumidityValue); ok {
					tsHum = time.Now()
				}
			}
		}
	}()

	// Test cycles
	cycle := 0
	for {
		cycle++
		o.println("=== boardtest: cycle ", cycle, " ===")

		// Sequence UP (front to back)
		for _, name := range powerSeq {
			setRail(ui, name, true)
			o.println("rail up: ", name)
			time.Sleep(stepDelayUp)
		}
		time.Sleep(dwellUp)

		// Sequence DOWN (back to front)
		for i := len(powerSeq) - 1; i >= 0; i-- {
			name := powerSeq[i]
			setRail(ui, name, false)
			o.println("rail down: ", name)
			time.Sleep(stepDelayDown)
		}
		time.Sleep(dwellDown)

		// Assess freshness
		now := time.Now()
		miss := make([]string, 0, 4)
		if tsBatt.IsZero() || now.Sub(tsBatt) > freshMaxAge {
			miss = append(miss, "battery")
		}
		if tsChg.IsZero() || now.Sub(tsChg) > freshMaxAge {
			miss = append(miss, "charger")
		}
		if tsTmp.IsZero() || now.Sub(tsTmp) > freshMaxAge {
			miss = append(miss, "temperature")
		}
		if tsHum.IsZero() || now.Sub(tsHum) > freshMaxAge {
			miss = append(miss, "humidity")
		}

		pass := len(miss) == 0
		if pass {
			o.println("[PASS] rails toggled; LTC4015 + AHT20 values observed recently")
		} else {
			o.println("[FAIL] missing or stale: ", fmt.Sprintf("%v", miss))
		}
		ledFlashPassFail(ui, pass)

		if cyclesToRun > 0 && cycle >= cyclesToRun {
			o.println("completed ", cycle, " cycles; halting")
			return
		}
	}
}

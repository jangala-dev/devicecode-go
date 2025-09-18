// Command pico-demo: HAL bring-up for RP2040/Pico with LTC4015 + AHT20 + UART echo.
//
// Build/flash (TinyGo):
//   tinygo flash -target pico ./services/hal/cmd/pico-demo
//
// Wiring assumptions (edit in halCfg as needed):
// - I2C0 @ 400 kHz on Pico defaults: SDA=GP4, SCL=GP5.
// - AHT20 on I2C address 0x38.
// - LTC4015 on I2C address 0x67; SMBALERT# wired to GP22 (active-low).
// - UART0 pins per your board config (baud/format set in halCfg).

package main

import (
	"context"
	"fmt"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"

	// Register device adaptors.
	_ "devicecode-go/services/hal/internal/devices/aht20adpt"
	_ "devicecode-go/services/hal/internal/devices/ltc4015adpt"
	_ "devicecode-go/services/hal/internal/devices/uartadpt"
)

func main() {
	time.Sleep(3 * time.Second)
	fmt.Println("\n== Jangala devicecode: Pico demo (HAL + LTC4015 + AHT20 + UART echo) ==")

	// In-process bus and connection.
	b := bus.NewBus(64)
	conn := b.NewConnection("main")

	// Subscriptions.
	stateSub := conn.Subscribe(bus.T("hal", "state"))
	defer conn.Unsubscribe(stateSub)
	allCaps := conn.Subscribe(bus.T("hal", "capability", "#"))
	defer conn.Unsubscribe(allCaps)

	// Start HAL.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hal.Run(ctx, conn)

	// Wait until HAL is ready for config.
	waitForAwaitingConfig(stateSub)

	// ----------------------------------------------------------------------------
	// EDITABLE CONFIGURATION
	// ----------------------------------------------------------------------------
	halCfg := types.HALConfig{
		Devices: []types.Device{
			// LTC4015 charger / power telemetry (I2C).
			{
				ID:   "charger0",
				Type: "ltc4015",
				BusRef: types.BusRef{
					Type: "i2c",
					ID:   "i2c0", //each bus is wired to its pico defaults as given in machine package constants
				},
				Params: map[string]any{
					// Electrical design parameters (edit to match PCB):
					"addr":             0x67, // I2C 7-bit address
					"cells":            6,
					"chem":             "lead_acid",
					"rsnsb_uohm":       1500, // battery sense (µΩ)
					"rsnsi_uohm":       1000, // input sense (µΩ)
					"targets_writable": true,
					"qcount_prescale":  1024,

					// Runtime / IRQ:
					"sample_every_ms": 1000, // ≥200 ms (as datasheet says)
					"smbalert_pin":    19,   // GP19; remove to disable IRQ
					"irq_debounce_ms": 2,

					// Convenience:
					"force_meas_sys_on": true,
					"enable_qcount":     true,
				},
			},

			// AHT20 temperature/humidity (I2C).
			{
				ID:   "env0",
				Type: "aht20",
				BusRef: types.BusRef{
					Type: "i2c",
					ID:   "i2c0",
				},
				Params: map[string]any{
					"addr": 0x38,
				},
			},

			// UART device for echo demo.
			{
				ID:   "uart_demo",
				Type: "uart",
				BusRef: types.BusRef{
					Type: "uart",
					ID:   "uart0", // Pico UART0
				},
				Params: map[string]any{
					"baud":          115200,
					"mode":          "bytes", // or "lines"
					"max_frame":     128,     // 16..256
					"idle_flush_ms": 100,     // used in "lines" mode
					"echo_tx":       false,   // keep false to avoid loop; we echo in software below
					// Optional format (supported on RP2):
					"databits": 8,
					"stopbits": 1,
					"parity":   "none", // "even" | "odd"
				},
			},
		},
	}
	// ----------------------------------------------------------------------------

	// Publish configuration.
	conn.Publish(conn.NewMessage(bus.T("config", "hal"), halCfg, false))
	fmt.Println("Config sent. Streaming telemetry...")

	// Start a simple UART echo server in a separate goroutine.
	go startUARTEcho(ctx, conn)

	// Print the first post-config state then continue printing.
	printHALStateFrom(stateSub)

	// Main print loop.
	for {
		select {
		case m := <-stateSub.Channel():
			printHALState(m)
		case m := <-allCaps.Channel():
			printCapability(m, conn)
		}
	}
}

// ---------------- UART echo server ----------------
//
// Subscribes to all UART events and, for each RX frame, issues a request–reply
// control call to write the same bytes back to that UART capability.
// Assumes you are only using this demo UART (or that echoing all is acceptable).

func startUARTEcho(ctx context.Context, conn *bus.Connection) {
	sub := conn.Subscribe(bus.T("hal", "capability", "uart", "+", "event"))
	defer conn.Unsubscribe(sub)

	fmt.Println("UART echo server: waiting for RX frames...")

	for {
		select {
		case <-ctx.Done():
			return
		case m := <-sub.Channel():
			ev, ok := m.Payload.(types.UARTEvent)
			if !ok {
				continue
			}
			// Only echo what we received (RX).
			if ev.Dir != types.UARTRx || ev.N == 0 {
				continue
			}

			// Extract numeric capability id from topic[3].
			if len(m.Topic) < 5 {
				continue
			}
			idTok := m.Topic[3]
			id, ok := tokAsInt(idTok)
			if !ok {
				continue
			}

			// Prepare control topic: hal/capability/uart/<id>/control/write
			ctrlTopic := bus.Topic{"hal", "capability", "uart", id, "control", "write"}
			req := conn.NewMessage(ctrlTopic, types.UARTWrite{Data: append([]byte(nil), ev.Data...)}, false)

			// Request–reply with a short timeout.
			cctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			reply, err := conn.RequestWait(cctx, req)
			cancel()
			if err != nil {
				fmt.Printf("UART echo: write failed: %v\n", err)
				continue
			}
			if rep, ok := reply.Payload.(types.UARTWriteReply); ok && rep.OK {
				fmt.Printf("UART echo: echoed %d bytes on uart/%d\n", rep.N, id)
			} else if er, ok := reply.Payload.(types.ErrorReply); ok {
				fmt.Printf("UART echo: error reply: %s\n", er.Error)
			} else {
				fmt.Printf("UART echo: unexpected reply payload: %#v\n", reply.Payload)
			}
		}
	}
}

// ---------------- Printing helpers ----------------

func waitForAwaitingConfig(sub *bus.Subscription) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case m := <-sub.Channel():
			if st, ok := m.Payload.(types.HALState); ok {
				fmt.Printf("[HAL %s] %s", st.TS.Format(time.RFC3339), summariseHALState(st))
				if st.Status == "awaiting_config" {
					return
				}
			}
		case <-timer.C:
			return
		}
	}
}

func printHALStateFrom(sub *bus.Subscription) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case m := <-sub.Channel():
			printHALState(m)
			return
		case <-timer.C:
			return
		}
	}
}

func printHALState(m *bus.Message) {
	st, ok := m.Payload.(types.HALState)
	if !ok {
		return
	}
	fmt.Printf("[HAL %s] %s", st.TS.Format(time.RFC3339), summariseHALState(st))
}

func summariseHALState(st types.HALState) string {
	if st.Error != "" {
		return fmt.Sprintf("state=%s status=%s error=%s\n", st.Level, st.Status, st.Error)
	}
	return fmt.Sprintf("state=%s status=%s\n", st.Level, st.Status)
}

func printCapability(m *bus.Message, _ *bus.Connection) {
	ts := time.Now().Format(time.RFC3339)
	kind, idStr, suffix := "-", "-", "-"
	if len(m.Topic) >= 5 {
		if s, _ := m.Topic[2].(string); s != "" {
			kind = s
		}
		idStr = tokString(m.Topic[3])
		if s, _ := m.Topic[4].(string); s != "" {
			suffix = s
		}
	}
	switch v := m.Payload.(type) {
	case types.TemperatureInfo:
		fmt.Printf("[%s] %s/%s info: temperature driver=%s unit=%s precision=%.1f\n",
			ts, kind, idStr, v.Driver, v.Unit, v.Precision)
	case types.HumidityInfo:
		fmt.Printf("[%s] %s/%s info: humidity driver=%s unit=%s precision=%.1f\n",
			ts, kind, idStr, v.Driver, v.Unit, v.Precision)
	case types.PowerInfo:
		fmt.Printf("[%s] %s/%s info: power driver=%s cells=%d chemistry=%s\n",
			ts, kind, idStr, v.Driver, v.Cells, chemString(v.Chemistry))
	case types.ChargerInfo:
		fmt.Printf("[%s] %s/%s info: charger model=%s cells=%d chemistry=%s targets_writable=%v\n",
			ts, kind, idStr, v.Model, v.Cells, chemString(v.Chemistry), v.TargetsWritable)
	case types.AlertsInfo:
		fmt.Printf("[%s] %s/%s info: alerts groups=%v\n", ts, kind, idStr, v.Groups)
	case types.UARTInfo:
		fmt.Printf("[%s] %s/%s info: uart driver=%s\n", ts, kind, idStr, v.Driver)

	case types.CapabilityState:
		if v.Error != "" {
			fmt.Printf("[%s] %s/%s state: %s error=%s\n", ts, kind, idStr, linkString(v.Link), v.Error)
		} else {
			fmt.Printf("[%s] %s/%s state: %s\n", ts, kind, idStr, linkString(v.Link))
		}

	case types.TemperatureValue:
		fmt.Printf("[%s] temperature/%s value: %.1f °C (ts=%s)\n",
			ts, idStr, float64(v.DeciC)/10.0, v.TS.Format(time.RFC3339))
	case types.HumidityValue:
		fmt.Printf("[%s] humidity/%s value: %.1f %%RH (ts=%s)\n",
			ts, idStr, float64(v.DeciPercent)/10.0, v.TS.Format(time.RFC3339))
	case types.PowerValue:
		fmt.Printf("[%s] power/%s value:", ts, idStr)
		if v.VBatPerCell_mV != nil {
			fmt.Printf(" Vbat/cell=%dmV", *v.VBatPerCell_mV)
		}
		if v.VBatPack_mV != nil {
			fmt.Printf(" Vbat/pack=%dmV", *v.VBatPack_mV)
		}
		if v.Vin_mV != nil {
			fmt.Printf(" Vin=%dmV", *v.Vin_mV)
		}
		if v.Vsys_mV != nil {
			fmt.Printf(" Vsys=%dmV", *v.Vsys_mV)
		}
		if v.IBat_mA != nil {
			fmt.Printf(" Ibat=%dmA", *v.IBat_mA)
		}
		if v.IIn_mA != nil {
			fmt.Printf(" Iin=%dmA", *v.IIn_mA)
		}
		if v.Die_mC != nil {
			fmt.Printf(" Tdie=%.1f°C", float64(*v.Die_mC)/1000.0)
		}
		if v.BSR_uohmPerCell != nil {
			fmt.Printf(" BSR/cell=%dµΩ", *v.BSR_uohmPerCell)
		}
		if v.IChargeDAC_mA != nil {
			fmt.Printf(" Ichg(DAC)=%dmA", *v.IChargeDAC_mA)
		}
		if v.IInLimitDAC_mA != nil {
			fmt.Printf(" IinLimit(DAC)=%dmA", *v.IInLimitDAC_mA)
		}
		if v.IChargeBSR_mA != nil {
			fmt.Printf(" Ichg(BSR)=%dmA", *v.IChargeBSR_mA)
		}
		fmt.Printf(" (ts=%s)\n", v.TS.Format(time.RFC3339))
	case types.ChargerValue:
		fmt.Printf("[%s] charger/%s value: phase=%s ok_to_charge=%v input_limited{vin_uvcl=%v iin_limit=%v} faults{bat_missing=%v bat_short=%v thermal_shutdown=%v} raw{ss=0x%04x cs=0x%04x st=0x%04x} (ts=%s)\n",
			ts, idStr,
			phaseString(v.Phase),
			v.OKToCharge,
			v.InputLimited.VinUvcl, v.InputLimited.IInLimit,
			v.Faults.BatMissing, v.Faults.BatShort, v.Faults.ThermalShutdown,
			v.Raw.SystemStatus, v.Raw.ChargerState, v.Raw.ChargeStatus,
			v.TS.Format(time.RFC3339),
		)
	case types.AlertsEvent:
		if v.Limit != 0 || v.ChgState != 0 || v.ChgStatus != 0 {
			fmt.Printf("[%s] alerts/%s event: limit=0x%04x chg_state=0x%04x chg_status=0x%04x (ts=%s)\n",
				ts, idStr, v.Limit, v.ChgState, v.ChgStatus, v.TS.Format(time.RFC3339))
		}
	case types.UARTEvent:
		dir := map[types.UARTDir]string{types.UARTTx: "tx", types.UARTRx: "rx"}[v.Dir]
		fmt.Printf("[%s] uart/%s %s: %d bytes\n", ts, idStr, dir, v.N)

	default:
		fmt.Printf("[%s] %s/%s/%s payload: %#v\n", ts, kind, idStr, suffix, v)
	}
}

// ---------------- Small utilities ----------------

func tokAsInt(t any) (int, bool) {
	switch v := t.(type) {
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	default:
		return 0, false
	}
}

func tokString(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case int:
		return itoa(v)
	case int32:
		return itoa(int(v))
	case int64:
		return itoa(int(v))
	case uint:
		return itoa(int(v))
	case uint32:
		return itoa(int(v))
	case uint64:
		return itoa(int(v))
	default:
		return fmt.Sprintf("%v", v)
	}
}

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

func chemString(c types.Chemistry) string {
	switch c {
	case types.ChemLithium:
		return "lithium"
	case types.ChemLeadAcid:
		return "lead_acid"
	default:
		return "unknown"
	}
}

func phaseString(p types.ChargerPhase) string {
	switch p {
	case types.PhasePrecharge:
		return "precharge"
	case types.PhaseCC:
		return "cc"
	case types.PhaseCV:
		return "cv"
	case types.PhaseAbsorb:
		return "absorb"
	case types.PhaseEqualize:
		return "equalize"
	case types.PhaseSuspended:
		return "suspended"
	case types.PhaseFault:
		return "fault"
	default:
		return "idle"
	}
}

func linkString(l types.LinkState) string {
	switch l {
	case types.LinkUp:
		return "up"
	case types.LinkDegraded:
		return "degraded"
	default:
		return "down"
	}
}

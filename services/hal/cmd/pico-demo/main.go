// Command pico-demo: minimal HAL bring-up for RP2040/Pico with LTC4015 + AHT20.
// Put this file under: services/hal/cmd/pico-demo/main.go
//
// Build/flash (TinyGo):
//   tinygo flash -target pico ./services/hal/cmd/pico-demo
//
// Wiring assumptions (edit to match your board):
// - I2C0 at 400 kHz on Pico defaults: SDA=GP4, SCL=GP5 (TinyGo machine defaults).
// - AHT20 on I2C address 0x38.
// - LTC4015 on I2C address 0x67 (adjust to your board; many designs use 0x67/0x68).
// - SMBALERT# from LTC4015 wired to GP22 (active-low). Change as needed below.
//
// Notes for EEs:
// - Tweak only the `halCfg` block; everything else is plumbing.
// - All values are in the units stated in-line. If unsure, keep defaults.
// - Sense resistor values (RSNSB/RSNSI) must match the actual hardware.

package main

import (
	"context"
	"fmt"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"

	// Device adaptors are registered via init() in these packages.
	// This main must live under services/hal/... so these internal imports are legal.
	_ "devicecode-go/services/hal/internal/devices/aht20adpt"
	_ "devicecode-go/services/hal/internal/devices/ltc4015adpt"
)

func main() {
	// Small delay to give the USB-CDC console a moment to enumerate on first boot.
	time.Sleep(3 * time.Second)

	fmt.Println("\n== Jangala devicecode: Pico demo (HAL + LTC4015 + AHT20) ==")

	// Create in-process bus and connection.
	b := bus.NewBus(4)
	conn := b.NewConnection("main")

	// Subscribe to HAL state early so we can see "awaiting_config".
	stateSub := conn.Subscribe(bus.T("hal", "state"))
	defer conn.Unsubscribe(stateSub)

	allSub := conn.Subscribe(bus.T("hal", "capability", "#"))
	defer conn.Unsubscribe(allSub)

	// Start HAL service.
	ctx := context.Background()
	go hal.Run(ctx, conn)

	// Wait until HAL reports it's ready for configuration (best-effort).
	waitForAwaitingConfig(stateSub)

	// ---- EDIT BELOW: Hardware configuration for your board ----
	// I2C bus IDs: "i2c0" or "i2c1" (as provided by the platform factory).
	// GPIO numbers are Pico GP numbers (0..28).
	halCfg := types.HALConfig{
		Devices: []types.Device{
			{
				ID:   "charger0",
				Type: "ltc4015",
				BusRef: types.BusRef{
					Type: "i2c",
					ID:   "i2c0",
				},
				// Params are decoded by the LTC4015 adaptor.
				// See services/hal/internal/devices/ltc4015/adaptor.go (Params struct).
				Params: map[string]any{
					// -- Electrical / design-time parameters --
					"addr":             0x67, // I²C address (7-bit)
					"cells":            6,    // Lead-acid 12V nominal = 6 cells
					"chem":             "lead_acid",
					"rsnsb_uohm":       1500, // Battery sense resistor (µΩ) — edit to match PCB
					"rsnsi_uohm":       1000, // Input sense resistor (µΩ) — edit to match PCB
					"targets_writable": true, // Allow profile/target updates at runtime
					"qcount_prescale":  1024, // If using coulomb counting (optional)

					// -- Sampling / IRQ / quality-of-life --
					"sample_every_ms": 1000, // Telemetry period (ms), clamped ≥200ms
					"smbalert_pin":    22,   // GP number for SMBALERT# (active-low), or omit to disable IRQ
					"irq_debounce_ms": 2,    // Debounce for the alert GPIO (ms)

					// -- Convenience flags; safe to leave as-is --
					"force_meas_sys_on": true, // Ensure measurement system active
					"enable_qcount":     true, // Enable coulomb counter if wired
				},
			},
			{
				ID:   "env0",
				Type: "aht20",
				BusRef: types.BusRef{
					Type: "i2c",
					ID:   "i2c0",
				},
				// AHT20 parameters (address only; defaults to 0x38 if omitted).
				Params: map[string]any{
					"addr": 0x38,
				},
			},
		},
	}
	// ---- END OF EDITABLE SECTION ----

	// Publish configuration to HAL.
	conn.Publish(conn.NewMessage(bus.T("config", "hal"), halCfg, false))
	fmt.Println("Config sent. Streaming telemetry...")

	// Print the first state we see after config, then carry on printing everything else.
	printHALStateFrom(stateSub)

	// Main print loop.
	for {
		select {
		case m := <-stateSub.Channel():
			printHALState(m)
		case m := <-allSub.Channel():
			printCapability(m)
		}
	}
}

// ----- Printing helpers -----

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

func printCapability(m *bus.Message) {
	ts := time.Now().Format(time.RFC3339)
	// Topic format: hal/capability/<kind>/<id>/<suffix>
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
	// --- Info (retained) ---
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

	// --- State (retained) ---
	case types.CapabilityState:
		if v.Error != "" {
			fmt.Printf("[%s] %s/%s state: %s error=%s\n", ts, kind, idStr, linkString(v.Link), v.Error)
		} else {
			fmt.Printf("[%s] %s/%s state: %s\n", ts, kind, idStr, linkString(v.Link))
		}

	// --- Values / events ---
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

	// --- GPIO/UART (not used in this demo, left for completeness) ---
	case types.UARTEvent:
		dir := map[types.UARTDir]string{types.UARTTx: "tx", types.UARTRx: "rx"}[v.Dir]
		fmt.Printf("[%s] uart/%s %s: %d bytes\n", ts, idStr, dir, v.N)

	default:
		// Unknown/other: print a compact summary.
		fmt.Printf("[%s] %s/%s/%s payload: %#v\n", ts, kind, idStr, suffix, v)
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

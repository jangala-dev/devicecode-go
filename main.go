package main

import (
	"machine"
	"sync/atomic"
	"time"

	"devicecode-go/drivers/ltc4015"
)

const smbPin = machine.GP15

// Buffered channel for ISR -> main signalling (ISR must not block).
var alertCh = make(chan struct{}, 4)
var dropped uint32 // count ISR sends that could not enqueue

func main() {
	// Allow USB CDC to enumerate before we print.
	time.Sleep(2 * time.Second)
	println("boot")

	// I2C0 @ 400 kHz on Pico defaults.
	machine.I2C0.Configure(machine.I2CConfig{
		Frequency: 400 * machine.KHz,
		SDA:       machine.I2C0_SDA_PIN,
		SCL:       machine.I2C0_SCL_PIN,
	})

	// SMBALERT# pin (open-drain, active-low) with pull-up.
	// If the board already has a pull-up, PinInput is sufficient.
	smbPin.Configure(machine.PinConfig{Mode: machine.PinInputPullup})
	if err := smbPin.SetInterrupt(machine.PinFalling, func(machine.Pin) {
		select {
		case alertCh <- struct{}{}:
		default:
			atomic.AddUint32(&dropped, 1)
		}
	}); err != nil {
		println("SetInterrupt:", err.Error())
	}

	// LTC4015 device with configuration.
	// Known chemistry → use New (not NewAuto).
	dev := ltc4015.New(machine.I2C0, ltc4015.Config{
		RSNSB_uOhm: 3330, // 0.00333 Ω
		RSNSI_uOhm: 1670, // 0.00167 Ω
		Cells:      6,
		Chem:       ltc4015.ChemLeadAcid,
	})

	// Keep telemetry running and enable.
	_ = dev.SetConfigBits(ltc4015.CfgForceMeasSysOn | ltc4015.CfgEnableQCount)

	// 1) Disable non-charger groups explicitly and clear any latches.
	_ = dev.EnableLimitAlertsMask(0)
	_ = dev.EnableChargerStateAlertsMask(0)
	_ = dev.ClearLimitAlerts()
	_ = dev.ClearChargerStateAlerts()

	// 2) Charger-status only; enable edge-triggered behaviour.
	// Enable only bits that are not currently active, then clear latches.
	enabled := baseMask()
	if cur, err := dev.ChargeStatus(); err == nil {
		enabled &^= cur
	}
	_ = dev.EnableChargeStatusAlertsMask(enabled)
	_ = dev.ClearChargeStatusAlerts()

	// If ALERT# is already low at boot, service once (use driver polarity helper).
	if dev.AlertActive(func() bool { return smbPin.Get() }) {
		select {
		case alertCh <- struct{}{}:
		default:
		}
	}

	// Periodic stats.
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	println("SMBALERT# armed on GP15; edge-triggered charger-status alerts")

	for {
		select {
		case <-alertCh:
			serviceChgStatus(dev, &enabled, smbPin)

		case <-tick.C:
			if ok, err := dev.MeasSystemValid(); err == nil && ok {
				printStats(dev)
			}
		}
	}
}

func serviceChgStatus(dev *ltc4015.Device, enabled *ltc4015.ChargeStatusEnable, pin machine.Pin) {
	// Drain until ALERT# deasserts, with a generous safety cap.
	const maxIters = 64
	for iters := 0; !pin.Get() && iters < maxIters; iters++ {
		// ARA + drain alerts (driver also clears latches).
		ev, ok, err := dev.ServiceSMBAlert()
		if err != nil {
			// If the line rose while we were starting ARA, treat as drained.
			if pin.Get() {
				return
			}
			println("ServiceSMBAlert:", err.Error())
			return
		}
		if !ok {
			// Another device responded; brief settle then re-check the line.
			time.Sleep(2 * time.Millisecond)
			continue
		}

		// Report newly-active CHARGE_STATUS bits we were watching.
		newActive := ev.ChgStatus & *enabled
		reportChgStatus(newActive)

		// Recompute enable mask so we only trigger on future transitions.
		if s, err := dev.ChargeStatus(); err == nil {
			newEn := baseMask() &^ s
			if newEn != *enabled {
				*enabled = newEn
				_ = dev.EnableChargeStatusAlertsMask(*enabled) // write only on change
			}
		}

		// Small settle to allow any subsequent alert to assert.
		time.Sleep(2 * time.Millisecond)
	}

	// Still asserted after our safety cap? Re-queue a follow-up attempt to avoid starvation
	if !pin.Get() {
		time.Sleep(2 * time.Millisecond)
		select {
		case alertCh <- struct{}{}:
		default:
			atomic.AddUint32(&dropped, 1)
		}
	}
}

func baseMask() ltc4015.ChargeStatusEnable {
	return ltc4015.CsVinUvclActive |
		ltc4015.CsIinLimitActive |
		ltc4015.CsConstCurrent |
		ltc4015.CsConstVoltage
}

func reportChgStatus(s ltc4015.ChargeStatusAlerts) {
	if s == 0 {
		return
	}
	if s.Has(ltc4015.CsVinUvclActive) {
		println("STATUS: VIN UVCL active")
	}
	if s.Has(ltc4015.CsIinLimitActive) {
		println("STATUS: input current limited")
	}
	if s.Has(ltc4015.CsConstCurrent) {
		println("STATUS: CC phase")
	}
	if s.Has(ltc4015.CsConstVoltage) {
		println("STATUS: CV phase")
	}
}

func printStats(dev *ltc4015.Device) {
	if mv, err := dev.VinMilliV(); err == nil {
		print("VIN: ", mv, " | ")
	}
	if mv, err := dev.VsysMilliV(); err == nil {
		print("VSYS: ", mv, " | ")
	}
	if mv, err := dev.BatteryMilliVPack(); err == nil {
		print("VBAT: ", mv, " | ")
	}
	if ma, err := dev.IinMilliA(); err == nil {
		print("IIN: ", ma, " | ")
	}
	if ma, err := dev.IbatMilliA(); err == nil {
		print("IBAT: ", ma, " | ")
	}
	if mC, err := dev.DieMilliC(); err == nil {
		print("DIE mC: ", mC, " | ")
	}

	// Optional context: effective limits.
	// if ma, err := dev.IinLimitDAC_mA(); err == nil {
	// 	print("IIN_LIMIT_DAC: ", ma, " | ")
	// }
	// if ma, err := dev.IChargeDAC_mA(); err == nil {
	// 	print("ICHARGE_DAC: ", ma, " | ")
	// }

	// ISR drop count.
	print("ISR drops: ", atomic.LoadUint32(&dropped))

	print("\n")
}

some code demonstrating the use of the driver:

```
package main

import (
	"machine"
	"sync/atomic"
	"time"

	"devicecode-go/drivers/ltc4015"
)

const (
	smbPin = machine.GP15

	// VIN window thresholds (millivolts).
	vinLo_mV = 9000
	vinHi_mV = 11000

	// BSR guards.
	minDeltaI_mA        = 500    // trigger BSR only if |IBAT| ≥ this
	bsrOpen_uOhmPerCell = 100000 // alert threshold for “open battery”
	triggerEveryNTicks  = 4
)

// Buffered channel for ISR -> main signalling (ISR must not block).
var alertCh = make(chan struct{}, 8)
var dropped uint32 // count ISR sends that could not enqueue

func main() {
	time.Sleep(2 * time.Second)
	println("boot")

	// I2C0 @ 400 kHz on Pico defaults.
	machine.I2C0.Configure(machine.I2CConfig{
		Frequency: 400 * machine.KHz,
		SDA:       machine.I2C0_SDA_PIN,
		SCL:       machine.I2C0_SCL_PIN,
	})

	// SMBALERT# pin (open-drain, active-low) with pull-up.
	smb := machine.Pin(smbPin)
	smb.Configure(machine.PinConfig{Mode: machine.PinInputPullup})
	if err := smb.SetInterrupt(machine.PinFalling, func(machine.Pin) {
		select {
		case alertCh <- struct{}{}:
		default:
			atomic.AddUint32(&dropped, 1)
		}
	}); err != nil {
		println("SetInterrupt:", err.Error())
	}

	// LTC4015 device with configuration.
	dev := ltc4015.New(machine.I2C0, ltc4015.Config{
		RSNSB_uOhm: 3330, // 0.00333 Ω
		RSNSI_uOhm: 1670, // 0.00167 Ω
		Cells:      6,
		Chem:       ltc4015.ChemLeadAcid,
	})

	// Keep telemetry running and enable coulomb counter.
	_ = dev.SetConfigBits(ltc4015.ForceMeasSysOn | ltc4015.EnableQCount)

	// ---------- VIN window and event-driven alert enabling ----------
	if err := dev.SetVINWindow_mV(vinLo_mV, vinHi_mV); err != nil {
		println("SetVINWindow_mV:", err.Error())
	}
	// Arm high-BSR alert (persistently enabled via setVinEdgeMask below).
	_ = dev.SetBSRHigh_uOhmPerCell(bsrOpen_uOhmPerCell)

	_ = dev.ClearLimitAlerts()

	// Derive initial VIN state and arm only the next edge to avoid floods,
	// while keeping LaBSRHi enabled persistently.
	vinConnected := initVINStateAndMask(dev)

	// Charger state alerts: enable “battery missing fault” for clarity here.
	_ = dev.EnableChargerStateAlertsMask(ltc4015.BatMissingFault)
	_ = dev.ClearChargerStateAlerts()

	// Charger-status edge-driven behaviour as before.
	enabled := baseMask()
	if cur, err := dev.ChargeStatus(); err == nil {
		enabled &^= cur
	}
	_ = dev.EnableChargeStatusAlertsMask(enabled)
	_ = dev.ClearChargeStatusAlerts()

	// If ALERT# is already low at boot, service once.
	if dev.AlertActive(func() bool { return smb.Get() }) {
		select {
		case alertCh <- struct{}{}:
		default:
		}
	}

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	var tickN uint32

	println("SMBALERT# armed; VIN edge-driven alerts enabled")

	for {
		select {
		case <-alertCh:
			println("ALERT!")
			serviceAlerts(dev, &enabled, &vinConnected, smb)

		case <-tick.C:
			tickN++

			// Print rich information on every tick (only if telemetry valid).
			if ok, err := dev.MeasSystemValid(); err == nil && ok {
				printStateAndStatus(dev)
				printStats(dev)
			}

			// Every Nth tick, consider triggering BSR under guards.
			if tickN%triggerEveryNTicks == 0 {
				// Skip if a BSR request is already in flight.
				if cfg, err := dev.ReadConfig(); err == nil && cfg.Has(ltc4015.RunBSR) {
					continue
				}
				// Only attempt if actively charging and not input-limited.
				if st, err := dev.ChargeStatus(); err == nil {
					activePhase := st.Has(ltc4015.ConstCurrent) || st.Has(ltc4015.ConstVoltage)
					inputLimited := st.Has(ltc4015.IinLimitActive) || st.Has(ltc4015.VinUvclActive)
					if activePhase && !inputLimited {
						// Ensure charge current magnitude is sufficient.
						// if ibat_mA, err := dev.IbatMilliA(); err == nil && abs32(ibat_mA) >= minDeltaI_mA {
						if err := dev.SetConfigBits(ltc4015.RunBSR); err != nil {
							println("Trigger BSR:", err.Error())
						}
						// }
					}
				}
			}
		}
	}
}

// Determine initial VIN state and set the VIN alert enables to the opposite edge,
// keeping LaBSRHi enabled persistently.
func initVINStateAndMask(dev *ltc4015.Device) bool {
	mv, err := dev.VinMilliV()
	if err != nil {
		// On read error, be conservative: enable both edges (and keep LaBSRHi).
		setVinEdgeMask(dev, ltc4015.VINLo|ltc4015.VINHi)
		return false
	}

	switch {
	case mv >= vinHi_mV:
		// Currently connected: arm only the low-edge next.
		setVinEdgeMask(dev, ltc4015.VINLo)
		return true
	case mv <= vinLo_mV:
		// Currently disconnected: arm only the high-edge next.
		setVinEdgeMask(dev, ltc4015.VINHi)
		return false
	default:
		// In the window: arm both; first crossing will set the state.
		setVinEdgeMask(dev, ltc4015.VINLo|ltc4015.VINHi)
		return mv >= vinHi_mV // conservative initialisation
	}
}

// Ensure LaBSRHi is always enabled alongside the requested VIN edge mask.
func setVinEdgeMask(dev *ltc4015.Device, mask ltc4015.LimitEnable) {
	mask |= ltc4015.BSRHi
	_ = dev.EnableLimitAlertsMask(mask)
}

// Handle CHARGE_STATUS edges and VIN limit events with software edge arming.
func serviceAlerts(dev *ltc4015.Device, enabled *ltc4015.ChargeStatusEnable, vinConnected *bool, pin machine.Pin) {
	const maxIters = 64
	for iters := 0; !pin.Get() && iters < maxIters; iters++ {
		ev, ok, err := dev.ServiceSMBAlert()
		printAlertEvent(ev)

		if err != nil {
			if pin.Get() {
				return
			}
			println("ServiceSMBAlert:", err.Error())
			return
		}
		if !ok {
			time.Sleep(2 * time.Millisecond)
			continue
		}

		// --- VIN events: edge-driven re-arming to prevent floods ---
		if vinBits := ev.Limit & (ltc4015.VINLo | ltc4015.VINHi); vinBits != 0 {
			mv, _ := dev.VinMilliV() // resolve simultaneous latch and set final state

			if mv >= vinHi_mV && !*vinConnected {
				*vinConnected = true
				onVinConnected()
				setVinEdgeMask(dev, ltc4015.VINLo)
			} else if mv <= vinLo_mV && *vinConnected {
				*vinConnected = false
				onVinDisconnected()
				setVinEdgeMask(dev, ltc4015.VINHi)
			} else if mv > vinLo_mV && mv < vinHi_mV {
				setVinEdgeMask(dev, ltc4015.VINLo|ltc4015.VINHi)
			}
		}

		// --- CHARGE_STATUS edges (unchanged) ---
		if newActive := ev.ChgStatus & *enabled; newActive != 0 {
			reportChgStatus(newActive)
		}
		if s, err := dev.ChargeStatus(); err == nil {
			newEn := baseMask() &^ s
			if newEn != *enabled {
				*enabled = newEn
				_ = dev.EnableChargeStatusAlertsMask(*enabled)
			}
		}

		time.Sleep(2 * time.Millisecond)
	}

	// Still asserted after our safety cap? Re-queue a follow-up attempt.
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
	return ltc4015.VinUvclActive |
		ltc4015.IinLimitActive |
		ltc4015.ConstCurrent |
		ltc4015.ConstVoltage
}

func onVinConnected()    { println("EVENT: VIN connected") }
func onVinDisconnected() { println("EVENT: VIN disconnected") }

func reportChgStatus(s ltc4015.ChargeStatusAlerts) {
	if s == 0 {
		return
	}
	if s.Has(ltc4015.VinUvclActive) {
		println("STATUS: VIN UVCL active")
	}
	if s.Has(ltc4015.IinLimitActive) {
		println("STATUS: input current limited")
	}
	if s.Has(ltc4015.ConstCurrent) {
		println("STATUS: CC phase")
	}
	if s.Has(ltc4015.ConstVoltage) {
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
	if uohmPerCell, err := dev.BSRMicroOhmPerCell(); err == nil {
		print("BSR µΩ/cell: ", uohmPerCell)
		if cells := dev.Cells(); cells > 0 {
			// Optional: show an estimated pack value for convenience.
			pack := uint64(uohmPerCell) * uint64(cells)
			print(" (pack ≈ ", pack, ")")
		}
		print(" | ")
	}
	print("ISR drops: ", atomic.LoadUint32(&dropped))
	print("\n")
}

func printStateAndStatus(dev *ltc4015.Device) {
	// --- Instantaneous state ---
	if st, err := dev.ChargerState(); err == nil {
		print("CHARGER_STATE: ", uint16(st), " [")
		if st.Has(ltc4015.EqualizeCharge) {
			print("equalize ")
		}
		if st.Has(ltc4015.AbsorbCharge) {
			print("absorb ")
		}
		if st.Has(ltc4015.ChargerSuspended) {
			print("suspended ")
		}
		if st.Has(ltc4015.Precharge) {
			print("precharge ")
		}
		if st.Has(ltc4015.CCCVCharge) {
			print("cccv ")
		}
		if st.Has(ltc4015.NTCPause) {
			print("ntc_pause ")
		}
		if st.Has(ltc4015.TimerTerm) {
			print("timer_term ")
		}
		if st.Has(ltc4015.COverXTerm) {
			print("c_over_x_term ")
		}
		if st.Has(ltc4015.MaxChargeTimeFault) {
			print("max_charge_time_fault ")
		}
		if st.Has(ltc4015.BatMissingFault) {
			print("bat_missing_fault ")
		}
		if st.Has(ltc4015.BatShortFault) {
			print("bat_short_fault ")
		}
		print("] | ")
	}

	if cs, err := dev.ChargeStatus(); err == nil {
		print("CHARGE_STATUS: ", uint16(cs), " [")
		if cs.Has(ltc4015.VinUvclActive) {
			print("vin_uvcl_active ")
		}
		if cs.Has(ltc4015.IinLimitActive) {
			print("iin_limit_active ")
		}
		if cs.Has(ltc4015.ConstCurrent) {
			print("const_current ")
		}
		if cs.Has(ltc4015.ConstVoltage) {
			print("const_voltage ")
		}
		print("] | ")
	}

	if ss, err := dev.SystemStatus(); err == nil {
		print("SYSTEM_STATUS: ", uint16(ss), " [")
		if ss.Has(ltc4015.ChargerEnabled) {
			print("charger_enabled ")
		}
		if ss.Has(ltc4015.MpptEnPin) {
			print("mppt_en ")
		}
		if ss.Has(ltc4015.EqualizeReq) {
			print("equalize_req ")
		}
		if ss.Has(ltc4015.DrvccGood) {
			print("drvcc_good ")
		}
		if ss.Has(ltc4015.CellCountError) {
			print("cell_count_error ")
		}
		if ss.Has(ltc4015.OkToCharge) {
			print("ok_to_charge ")
		}
		if ss.Has(ltc4015.NoRt) {
			print("no_rt ")
		}
		if ss.Has(ltc4015.ThermalShutdown) {
			print("thermal_shutdown ")
		}
		if ss.Has(ltc4015.VinOvlo) {
			print("vin_ovlo ")
		}
		if ss.Has(ltc4015.VinGtVbat) {
			print("vin_gt_vbat ")
		}
		if ss.Has(ltc4015.IntvccGt4p3V) {
			print("intvcc_gt_4p3V ")
		}
		if ss.Has(ltc4015.IntvccGt2p8V) {
			print("intvcc_gt_2p8V ")
		}
		print("] | ")
	}

	// --- Optional: latched alerts (helpful to see edges/fault assertions) ---
	if csa, err := dev.ReadChargerStateAlerts(); err == nil && csa != 0 {
		print("CSA_ALERTS: ", uint16(csa), " [")
		if csa.Has(ltc4015.MaxChargeTimeFault) {
			print("max_charge_time_fault ")
		}
		if csa.Has(ltc4015.BatMissingFault) {
			print("bat_missing_fault ")
		}
		if csa.Has(ltc4015.BatShortFault) {
			print("bat_short_fault ")
		}
		if csa.Has(ltc4015.TimerTerm) {
			print("timer_term ")
		}
		if csa.Has(ltc4015.COverXTerm) {
			print("c_over_x_term ")
		}
		if csa.Has(ltc4015.NTCPause) {
			print("ntc_pause ")
		}
		if csa.Has(ltc4015.Precharge) {
			print("precharge ")
		}
		if csa.Has(ltc4015.CCCVCharge) {
			print("cccv ")
		}
		if csa.Has(ltc4015.ChargerSuspended) {
			print("suspended ")
		}
		if csa.Has(ltc4015.AbsorbCharge) {
			print("absorb ")
		}
		if csa.Has(ltc4015.EqualizeCharge) {
			print("equalize ")
		}
		print("] | ")
	}

	if css, err := dev.ReadChargeStatusAlerts(); err == nil && css != 0 {
		print("CS_ALERTS: ", uint16(css), " [")
		if css.Has(ltc4015.VinUvclActive) {
			print("vin_uvcl_active ")
		}
		if css.Has(ltc4015.IinLimitActive) {
			print("iin_limit_active ")
		}
		if css.Has(ltc4015.ConstCurrent) {
			print("const_current ")
		}
		if css.Has(ltc4015.ConstVoltage) {
			print("const_voltage ")
		}
		print("] | ")
	}
}

func printAlertEvent(ev ltc4015.AlertEvent) {
	if ev.Empty() {
		println("ALERT (no latches)")
		return
	}
	print("ALERT latches | ")

	if ev.Limit != 0 {
		print("LIMIT_ALERTS=0x", uint16(ev.Limit), " [")
		if ev.Limit.Has(ltc4015.MeasSysValid) {
			print("meas_sys_valid ")
		}
		if ev.Limit.Has(ltc4015.QCountLo) {
			print("qcount_lo ")
		}
		if ev.Limit.Has(ltc4015.QCountHi) {
			print("qcount_hi ")
		}
		if ev.Limit.Has(ltc4015.VBATLo) {
			print("vbat_lo ")
		}
		if ev.Limit.Has(ltc4015.VBATHi) {
			print("vbat_hi ")
		}
		if ev.Limit.Has(ltc4015.VINLo) {
			print("vin_lo ")
		}
		if ev.Limit.Has(ltc4015.VINHi) {
			print("vin_hi ")
		}
		if ev.Limit.Has(ltc4015.VSYSLo) {
			print("vsys_lo ")
		}
		if ev.Limit.Has(ltc4015.VSYSHi) {
			print("vsys_hi ")
		}
		if ev.Limit.Has(ltc4015.IINHi) {
			print("iin_hi ")
		}
		if ev.Limit.Has(ltc4015.IBATLo) {
			print("ibat_lo ")
		}
		if ev.Limit.Has(ltc4015.DieTempHi) {
			print("die_temp_hi ")
		}
		if ev.Limit.Has(ltc4015.BSRHi) {
			print("bsr_hi ")
		}
		if ev.Limit.Has(ltc4015.NTCRatioHi) {
			print("ntc_cold ")
		}
		if ev.Limit.Has(ltc4015.NTCRatioLo) {
			print("ntc_hot ")
		}
		print("| ")
	}

	if ev.ChgState != 0 {
		print("CHARGER_STATE_ALERTS=0x", uint16(ev.ChgState), " [")
		if ev.ChgState.Has(ltc4015.EqualizeCharge) {
			print("equalize ")
		}
		if ev.ChgState.Has(ltc4015.AbsorbCharge) {
			print("absorb ")
		}
		if ev.ChgState.Has(ltc4015.ChargerSuspended) {
			print("suspended ")
		}
		if ev.ChgState.Has(ltc4015.Precharge) {
			print("precharge ")
		}
		if ev.ChgState.Has(ltc4015.CCCVCharge) {
			print("cccv ")
		}
		if ev.ChgState.Has(ltc4015.NTCPause) {
			print("ntc_pause ")
		}
		if ev.ChgState.Has(ltc4015.TimerTerm) {
			print("timer_term ")
		}
		if ev.ChgState.Has(ltc4015.COverXTerm) {
			print("c_over_x_term ")
		}
		if ev.ChgState.Has(ltc4015.MaxChargeTimeFault) {
			print("max_charge_time_fault ")
		}
		if ev.ChgState.Has(ltc4015.BatMissingFault) {
			print("bat_missing_fault ")
		}
		if ev.ChgState.Has(ltc4015.BatShortFault) {
			print("bat_short_fault ")
		}
		print("| ")
	}

	if ev.ChgStatus != 0 {
		print("CHARGE_STATUS_ALERTS=0x", uint16(ev.ChgStatus), " [")
		if ev.ChgStatus.Has(ltc4015.VinUvclActive) {
			print("vin_uvcl_active ")
		}
		if ev.ChgStatus.Has(ltc4015.IinLimitActive) {
			print("iin_limit_active ")
		}
		if ev.ChgStatus.Has(ltc4015.ConstCurrent) {
			print("const_current ")
		}
		if ev.ChgStatus.Has(ltc4015.ConstVoltage) {
			print("const_voltage ")
		}
		print("| ")
	}
	print("\n")
}

// Richer periodic output, including BSR and ICHARGE_BSR with qualification.

func abs32(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}
```
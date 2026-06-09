//go:build !tinygo

package updater

// ProductionApplier returns the applier the reactor wires by default.
// On host builds (tests, dev environments without a flash slot to
// reboot into) this stays the safe-default RefusingApplier — commit
// returns commit_failed. Real reboot wiring lives in
// applier_tinygo.go.
func ProductionApplier() Applier { return RefusingApplier() }

func scheduleArmReboot(a Applier, d StagedDescriptor, results chan<- applyRebootResult) {
	if err := a.ArmReboot(d); err != nil {
		select {
		case results <- applyRebootResult{desc: d, err: err}:
		default:
		}
	}
}

//go:build tinygo && rp2350

package updater

import (
	"errors"
	"time"
)

// abupdateApplier reboots into the slot the abupdateSink staged into.
// CanApply requires that newSlotSink has previously initialised the
// shared updater (i.e. updater/main staging wrote a staged image); without
// that, the inactive slot still holds the previous image and rebooting
// would either roll back or fail at the bootloader.
type abupdateApplier struct{}

// ProductionApplier returns the abupdate-backed applier. CanApply
// validates that a staging cycle ran; ArmReboot calls
// abupdate.RebootIntoSlot which does not return on success.
func ProductionApplier() Applier { return abupdateApplier{} }

const postCommitReplyFlushDelay = 750 * time.Millisecond

func (abupdateApplier) CanApply(d StagedDescriptor) error {
	_ = d
	if !sharedUpdaterInit {
		return errors.New(ErrApplyUnavailable)
	}
	return nil
}

func (abupdateApplier) ArmReboot(d StagedDescriptor) error {
	_ = d
	if !sharedUpdaterInit {
		return errors.New("apply_reboot_failed:apply_unavailable_uninited")
	}
	// Does not return on success.
	rc := sharedUpdater.RebootIntoSlot()
	return errors.New("apply_reboot_failed:" + errFromRC("reboot_into_slot", rc).Error())
}

func scheduleArmReboot(a Applier, d StagedDescriptor, results chan<- applyRebootResult) {
	go func() {
		// handleCommit has only replied on the local bus. The fabric
		// session still needs a scheduler turn to marshal and write the
		// wire reply (and the state=rebooting retain) back to CM5 before
		// RebootIntoSlot stops the process.
		time.Sleep(postCommitReplyFlushDelay)
		if err := a.ArmReboot(d); err != nil {
			select {
			case results <- applyRebootResult{desc: d, err: err}:
			default:
			}
		}
	}()
}

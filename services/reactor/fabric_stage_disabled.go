//go:build !qa_reactor && !fabric_uart_hwtest && !fabric_stage_enabled

package reactor

import (
	"errors"

	"devicecode-go/services/fabric"
)

// rejectingFabricStageController is the default firmware transfer policy for
// this integration slice. It makes the Fabric transfer boundary explicit while
// guaranteeing that an unexpected xfer_begin cannot enter the TinyGo flash
// prestage path. Hardware cross-wire tests opt in to the updater-owned stage
// controller with the fabric_uart_hwtest build tag; production firmware can do
// the same later with fabric_stage_enabled once the flash path is ready.
type rejectingFabricStageController struct{}

func fabricTransferMode() string { return "stage-disabled" }

func (r *Reactor) fabricStageController() fabric.StageController {
	return rejectingFabricStageController{}
}

func (rejectingFabricStageController) BeginStreamedStage(xferID string, size uint32) (uint64, error) {
	_ = xferID
	_ = size
	return 0, errors.New("stage_disabled")
}

func (rejectingFabricStageController) WriteStreamedStage(xferID string, generation uint64, data []byte) error {
	_ = xferID
	_ = generation
	_ = data
	return errors.New("stage_disabled")
}

func (rejectingFabricStageController) CommitStreamedStage(xferID string, generation uint64) (uint32, error) {
	_ = xferID
	_ = generation
	return 0, errors.New("stage_disabled")
}

func (rejectingFabricStageController) AbortStreamedStage(xferID string, generation uint64, reason string) {
	_ = xferID
	_ = generation
	_ = reason
}

func (rejectingFabricStageController) CancelStreamedStage(xferID string, generation uint64, reason string) {
	_ = xferID
	_ = generation
	_ = reason
}

//go:build !qa_reactor && !fabric_uart_hwtest && fabric_stage_enabled

package reactor

import "devicecode-go/services/fabric"

func fabricTransferMode() string { return "stage-controller:flash-stage" }

func (r *Reactor) fabricStageController() fabric.StageController {
	if r == nil {
		return nil
	}
	return r.updaterSvc
}

//go:build !qa_reactor && fabric_uart_hwtest

package reactor

import "devicecode-go/services/fabric"

func fabricTransferMode() string { return "stage-controller:hwtest" }

func (r *Reactor) fabricStageController() fabric.StageController {
	if r == nil {
		return nil
	}
	return r.updaterSvc
}

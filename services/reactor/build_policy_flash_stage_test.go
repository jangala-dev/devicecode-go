//go:build !qa_reactor && !fabric_uart_hwtest && fabric_stage_enabled && !fabric_apply_enabled && !fabric_uart_selftest

package reactor

import "testing"

func TestBuildPolicyFlashStage(t *testing.T) {
	if got := fabricTransferMode(); got != "stage-controller:flash-stage" {
		t.Fatalf("fabricTransferMode() = %q", got)
	}
	if got := updaterRuntimeMode(); got != "safe-defaults:apply-disabled" {
		t.Fatalf("updaterRuntimeMode() = %q", got)
	}
	if !useHardwareFabricUART() {
		t.Fatalf("fabric_stage_enabled should use hardware Fabric UART")
	}
}

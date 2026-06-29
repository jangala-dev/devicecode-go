//go:build !qa_reactor && !fabric_stage_enabled && fabric_apply_enabled && !fabric_uart_hwtest && !fabric_uart_selftest

package reactor

import "testing"

func TestBuildPolicyApplyWithoutStageIsSafe(t *testing.T) {
	if got := fabricTransferMode(); got != "stage-disabled" {
		t.Fatalf("fabricTransferMode() = %q", got)
	}
	if got := updaterRuntimeMode(); got != "safe-defaults:apply-disabled" {
		t.Fatalf("updaterRuntimeMode() = %q", got)
	}
}

//go:build !qa_reactor && fabric_uart_hwtest && !fabric_uart_selftest

package reactor

import "testing"

func TestBuildPolicyUARTHWTest(t *testing.T) {
	if got := fabricTransferMode(); got != "stage-controller:hwtest" {
		t.Fatalf("fabricTransferMode() = %q", got)
	}
	if got := updaterRuntimeMode(); got != "safe-defaults:apply-disabled" {
		t.Fatalf("updaterRuntimeMode() = %q", got)
	}
	if !useHardwareFabricUART() {
		t.Fatalf("fabric_uart_hwtest should use hardware Fabric UART unless fabric_uart_selftest is also set")
	}
}

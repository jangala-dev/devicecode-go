package consts

import "testing"

func TestTokens(t *testing.T) {
	if TokConfig != "config" || TokHAL != "hal" || TokCapability != "capability" {
		t.Fatal("top-level tokens changed unexpectedly")
	}
	if CtrlReadNow != "read_now" || CtrlSetRate != "set_rate" {
		t.Fatal("control tokens changed unexpectedly")
	}
}

//go:build tinygo && rp2350

package updater

import (
	"github.com/jangala-dev/pico2-a-b/abupdate"
	"github.com/jangala-dev/pico2-a-b/signedimage"
)

func installABUpdateDiagHook(xferID string, generation uint64) {
	// Production firmware keeps flash/verifier observability in aggregate
	// updater counters and final stage results. Do not install per-block hooks.
	abupdate.ClearDiagnosticHook()
	signedimage.ClearDiagnosticHook()
}

func clearABUpdateDiagHook() {
	abupdate.ClearDiagnosticHook()
	signedimage.ClearDiagnosticHook()
}

func clearABUpdateDiagHookFor(xferID string, generation uint64) {
	clearABUpdateDiagHook()
}

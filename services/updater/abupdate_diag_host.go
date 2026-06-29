//go:build !tinygo || !rp2350

package updater

import "sync"

var abupdateDiagMu sync.Mutex
var abupdateDiagActive bool

func installABUpdateDiagHook(xferID string, generation uint64) {
	_, _ = xferID, generation
	abupdateDiagMu.Lock()
	abupdateDiagActive = true
	abupdateDiagMu.Unlock()
}

func clearABUpdateDiagHook() {
	abupdateDiagMu.Lock()
	abupdateDiagActive = false
	abupdateDiagMu.Unlock()
}

func abupdateDiagHookActiveForTest() bool {
	abupdateDiagMu.Lock()
	defer abupdateDiagMu.Unlock()
	return abupdateDiagActive
}

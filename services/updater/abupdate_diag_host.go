//go:build !tinygo || !rp2350

package updater

import "sync"

var abupdateDiagMu sync.Mutex
var abupdateDiagActive bool
var abupdateDiagXferID string
var abupdateDiagGeneration uint64

func installABUpdateDiagHook(xferID string, generation uint64) {
	abupdateDiagMu.Lock()
	abupdateDiagActive = true
	abupdateDiagXferID = xferID
	abupdateDiagGeneration = generation
	abupdateDiagMu.Unlock()
}

func clearABUpdateDiagHook() {
	abupdateDiagMu.Lock()
	abupdateDiagActive = false
	abupdateDiagXferID = ""
	abupdateDiagGeneration = 0
	abupdateDiagMu.Unlock()
}

func clearABUpdateDiagHookFor(xferID string, generation uint64) {
	abupdateDiagMu.Lock()
	if abupdateDiagActive && abupdateDiagXferID == xferID && abupdateDiagGeneration == generation {
		abupdateDiagActive = false
		abupdateDiagXferID = ""
		abupdateDiagGeneration = 0
	}
	abupdateDiagMu.Unlock()
}

func abupdateDiagHookActiveForTest() bool {
	abupdateDiagMu.Lock()
	defer abupdateDiagMu.Unlock()
	return abupdateDiagActive
}

package updater

import "sync/atomic"

func resetBootIDForTest() {
	cachedBootID.Store(nil)
	atomic.StoreUint64(&fallbackTick, 0)
}

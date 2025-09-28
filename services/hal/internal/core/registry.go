package core

import (
	"devicecode-go/types"
	"devicecode-go/x/fmtx"
	"sync"
)

var (
	regMu    sync.RWMutex
	builders = map[string]Builder{}
)

func RegisterBuilder(typ string, b Builder) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := builders[typ]; exists {
		panic(fmtx.Sprintf("duplicate device builder: %s", typ))
	}
	builders[typ] = b
}

func lookupBuilder(typ string) (Builder, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	b, ok := builders[typ]
	return b, ok
}

// Public HAL config type is in devicecode-go/types
type HALConfig = types.HALConfig
type HALDevice = types.HALDevice

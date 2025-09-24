//go:build rp2040

package platform

import (
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/platform/provider"
)

func GetResources() core.Resources {
	plan := GetSelectedPlan()                 // from setup_selected.go / setup_none.go
	reg := provider.NewResourceRegistry(plan) // pass plan into provider; no platform import in provider
	return core.Resources{
		Reg: reg,
		Pub: reg, // registry implements core.EventEmitter
	}
}

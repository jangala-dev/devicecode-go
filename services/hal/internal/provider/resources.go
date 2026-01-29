//go:build rp2040 || rp2350

package provider

import (
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/provider/setups"
)

// SelectedPlan and InitialHALConfig are provided via build-tagged files
// (see setup_selected.go / setup_none.go in this package).
// They are declared here for a single import surface.
var (
	SelectedPlan     setups.ResourcePlan
	InitialHALConfig core.HALConfig // alias to types.HALConfig via core
)

// NewResources constructs the registry from the selected plan.
// It replaces the former platform.GetResources.
func NewResources() core.Resources {
	reg := NewResourceRegistry(SelectedPlan)
	return core.Resources{Reg: reg}
}

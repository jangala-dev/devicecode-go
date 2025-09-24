package platform

import (
	"devicecode-go/services/hal/internal/platform/setups"
	"devicecode-go/types"
)

// Public accessors used by hal.Run and the provider.
func GetInitialConfig() types.HALConfig    { return getSelectedSetup() }
func GetSelectedPlan() setups.ResourcePlan { return getSelectedPlan() }

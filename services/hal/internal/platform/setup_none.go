//go:build !(pico && (pico_rich_dev || pico_bb_proto_1))

package platform

import (
	"devicecode-go/services/hal/internal/platform/setups"
	"devicecode-go/types"
)

func getSelectedSetup() types.HALConfig    { return types.HALConfig{} }
func getSelectedPlan() setups.ResourcePlan { return setups.ResourcePlan{} }

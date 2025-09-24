//go:build pico && (pico_rich_dev || pico_bb_proto_1)

package platform

import (
	"devicecode-go/services/hal/internal/platform/setups"
	"devicecode-go/types"
)

// Internal helpers used by setup.go to avoid duplicate public APIs.
func getSelectedSetup() types.HALConfig    { return setups.SelectedSetup }
func getSelectedPlan() setups.ResourcePlan { return setups.SelectedPlan }

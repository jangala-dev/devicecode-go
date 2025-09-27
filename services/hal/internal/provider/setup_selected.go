//go:build pico && (pico_rich_dev || pico_bb_proto_1)

package provider

import (
	"devicecode-go/services/hal/internal/provider/setups"
	"devicecode-go/types"
)

func init() {
	SelectedPlan = setups.SelectedPlan
	InitialHALConfig = types.HALConfig(setups.SelectedSetup)
}

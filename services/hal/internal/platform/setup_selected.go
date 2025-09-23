//go:build pico && (pico_rich_dev || pico_bb_proto_1)

package platform

import (
	"devicecode-go/services/hal/internal/platform/setups"
	"devicecode-go/types"
)

func getSelectedOrEmpty() types.HALConfig { return setups.SelectedSetup }

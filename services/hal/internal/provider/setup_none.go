//go:build !((rp2040 || rp2350) && (pico_rich_dev || pico_bb_proto_1))

package provider

import "devicecode-go/services/hal/internal/provider/setups"

func init() {
	SelectedPlan = setups.ResourcePlan{}
	// InitialHALConfig left zero-value (no devices).
}

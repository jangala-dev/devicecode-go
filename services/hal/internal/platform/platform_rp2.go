//go:build rp2040

package platform

import (
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/platform/provider"
)

func GetResources() core.Resources {
	return core.Resources{
		Pins: provider.NewPinFactory(),
	}
}

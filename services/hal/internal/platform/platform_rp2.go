//go:build rp2040

package platform

import (
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/platform/provider"

	_ "devicecode-go/services/hal/internal/platform/boards"
)

func GetResources() core.Resources {
	return core.Resources{
		Reg: provider.NewResourceRegistry(),
	}
}

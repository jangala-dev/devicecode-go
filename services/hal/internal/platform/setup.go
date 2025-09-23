package platform

import "devicecode-go/types"

func GetInitialConfig() types.HALConfig {
	return getSelectedOrEmpty()
}

package hal

import (
	"context"

	"devicecode-go/bus"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/platform"
)

func Run(ctx context.Context, conn *bus.Connection) {
	res := platform.GetResources()

	// If a compile-time setup is present, publish it as the initial config.
	if initCfg := platform.GetInitialConfig(); len(initCfg.Devices) > 0 {
		conn.Publish(conn.NewMessage(core.T("config", "hal"), initCfg, true))
	}

	h := core.NewHAL(conn, res)
	h.Run(ctx)
}

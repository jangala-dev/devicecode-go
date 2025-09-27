package hal

import (
	"context"

	"devicecode-go/bus"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/provider"
)

func Run(ctx context.Context, conn *bus.Connection) {
	res := provider.NewResources()

	// If a compile-time setup is present, publish it as the initial config.
	if initCfg := provider.InitialHALConfig; len(initCfg.Devices) > 0 { // new
		conn.Publish(conn.NewMessage(core.T("config", "hal"), initCfg, true))
	}

	h := core.NewHAL(conn, res)
	h.Run(ctx)
}

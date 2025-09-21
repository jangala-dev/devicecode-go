package hal

import (
	"context"

	"devicecode-go/bus"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/platform"
)

func Run(ctx context.Context, conn *bus.Connection) {
	res := platform.GetResources()
	h := core.NewHAL(conn, res)
	h.Run(ctx)
}

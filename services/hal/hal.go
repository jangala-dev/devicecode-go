package hal

import (
	"context"

	"devicecode-go/bus"
	"devicecode-go/services/hal/internal/core"
	"devicecode-go/services/hal/internal/platform"
)

// Run starts the HAL service (single non-blocking loop).
func Run(ctx context.Context, conn *bus.Connection) {
	res := platform.GetResources() // rp2040 provider selected via build tags
	h := core.NewHAL(conn, res)
	h.Run(ctx)
}

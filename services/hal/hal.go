// services/hal/hal.go
package hal

import (
	"context"

	"devicecode-go/bus"
	"devicecode-go/services/hal/internal/platform"
	"devicecode-go/services/hal/internal/service"
)

// Run starts the HAL service until ctx is cancelled.
// Device modules are brought in via blank imports elsewhere (e.g. in cmd/firmware).
func Run(ctx context.Context, conn *bus.Connection) {
	s := service.New(conn, platform.DefaultI2CFactory(), platform.DefaultPinFactory())
	s.Run(ctx)
}

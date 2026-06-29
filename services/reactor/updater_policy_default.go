//go:build !qa_reactor && (!fabric_stage_enabled || !fabric_apply_enabled || fabric_uart_hwtest || fabric_uart_selftest)

package reactor

import (
	"devicecode-go/bus"
	"devicecode-go/services/updater"
)

func updaterRuntimeMode() string { return "safe-defaults:apply-disabled" }

func updaterServiceOptions(conn *bus.Connection) updater.Options {
	return updater.Options{
		Conn:     conn,
		Identity: firmwareIdentity(),
	}
}

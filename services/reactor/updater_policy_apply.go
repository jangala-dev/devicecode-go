//go:build !qa_reactor && fabric_stage_enabled && fabric_apply_enabled && !fabric_uart_hwtest && !fabric_uart_selftest

package reactor

import (
	"devicecode-go/bus"
	"devicecode-go/services/updater"
)

func updaterRuntimeMode() string { return "production-applier:commit-reboots" }

func updaterServiceOptions(conn *bus.Connection) updater.Options {
	return updater.Options{
		Conn:     conn,
		Identity: firmwareIdentity(),
		Applier:  updater.ProductionApplier(),
	}
}

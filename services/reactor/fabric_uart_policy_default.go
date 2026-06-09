//go:build !qa_reactor && !fabric_uart_selftest

package reactor

func useHardwareFabricUART() bool { return true }

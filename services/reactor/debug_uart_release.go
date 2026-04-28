//go:build !debug_uart && !qa_reactor

package reactor

import "devicecode-go/bus"

// debugUARTLog is a no-op in release builds: the uart1 log mirror is
// disabled by default per docs/firmware-alignment-protocol.md (off in
// release, uart1-only in dev, rate-limited, never on uart0). Build with
// `-tags debug_uart` to enable; see debug_uart_dev.go.
type debugUARTLog struct{}

func (d *debugUARTLog) init(uiConn *bus.Connection)     { _ = uiConn }
func (d *debugUARTLog) openedChan() <-chan *bus.Message { return nil }
func (d *debugUARTLog) closedChan() <-chan *bus.Message { return nil }
func (d *debugUARTLog) handleOpened(m *bus.Message)     { _ = m }
func (d *debugUARTLog) handleClosed(uiConn *bus.Connection) {
	_ = uiConn
}

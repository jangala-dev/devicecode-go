//go:build !qa_reactor && (!fabric_uart_selftest || !fabric_uart_hwtest)

package reactor

func (r *Reactor) addFabricSelfTestChild() {}

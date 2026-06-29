// uartx/export.go

//go:build atmega || esp || nrf || sam || sifive || stm32 || k210 || nxp || rp2040 || rp2350

package uartx

import "machine"

type UARTConfig = machine.UARTConfig
type Pin = machine.Pin

const (
	NoPin        = machine.NoPin
	UART_TX_PIN  = machine.UART_TX_PIN
	UART_RX_PIN  = machine.UART_RX_PIN
	UART0_RX_PIN = machine.UART0_RX_PIN
	UART0_TX_PIN = machine.UART0_TX_PIN
	UART1_TX_PIN = machine.UART1_TX_PIN
	UART1_RX_PIN = machine.UART1_RX_PIN
)

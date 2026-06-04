// uartx/machine_rp2.go

//go:build rp2040 || rp2350

package uartx

import (
	"device/rp"
	"runtime/interrupt"
)

const deviceName = rp.Device

// UART on the RP2040/RP2035
var (
	UART0  = &_UART0
	_UART0 = UART{
		Bus: rp.UART0,
		// RX
		Buffer: NewRingBuffer(),
		notify: make(chan struct{}, 1),
		// TX
		TxBuffer: NewRingBuffer(),
		txNotify: make(chan struct{}, 1),
	}

	UART1  = &_UART1
	_UART1 = UART{
		Bus: rp.UART1,
		// RX
		Buffer: NewRingBuffer(),
		notify: make(chan struct{}, 1),
		// TX
		TxBuffer: NewRingBuffer(),
		txNotify: make(chan struct{}, 1),
	}
)

func init() {
	UART0.Interrupt = interrupt.New(rp.IRQ_UART0_IRQ, _UART0.handleInterrupt)
	UART1.Interrupt = interrupt.New(rp.IRQ_UART1_IRQ, _UART1.handleInterrupt)
}

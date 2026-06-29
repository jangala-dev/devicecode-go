// uartx/rp2_uart.go
//go:build rp2040 || rp2350

// Package uartx provides a minimal, interrupt-driven UART for RP2040/RP2350
// built around the PL011. Steady-state transmission is ISR-driven; the
// foreground only seeds the HW FIFO when it safely owns the TX start (TXIM
// masked) or performs a short “masked kick” when TXIM is enabled but no IRQ is
// pending and the FIFO is empty. This avoids foreground/ISR interleave while
// guaranteeing forward progress.
package uartx

import (
	"device/rp"
	"errors"
	"machine"
	"runtime/interrupt"
	"runtime/volatile"
)

// UART represents a single PL011 instance on RP2040/RP2350.
// Invariants (TX path):
//   - Steady-state writer to UARTDR is the ISR.
//   - Foreground writes UARTDR only when TXIM is masked (we own the start) or
//     in the “masked kick” corner case (TXIM enabled, no TX IRQ pending, FIFO empty).
//
// Signalling:
//   - txNotify is coalesced and used both for “writable” (space/progress) and
//     a final “drained” edge (SW empty and TXFE==1). Callers must re-check state.
type UART struct {
	// RX
	Buffer *RingBuffer    // software RX ring (compatible with TinyGo semantics)
	Bus    *rp.UART0_Type // PL011 register block

	// TX
	TxBuffer *RingBuffer   // software TX ring drained by the ISR
	txNotify chan struct{} // coalesced TX readiness/drain notifications

	Interrupt interrupt.Interrupt
	notify    chan struct{} // coalesced RX readiness notifications

	baud uint32 // last configured baud (for diagnostics, not used by HW)

	rxDrops    volatile.Register32
	rxOverruns volatile.Register32
	rxBreaks   volatile.Register32
	rxParity   volatile.Register32
	rxFraming  volatile.Register32
}

// Configure sets up the PL011, its pins and interrupts. It leaves RXIM/RTIM
// enabled and TXIM masked (enabled on demand by attemptSend).
func (uart *UART) Configure(cfg machine.UARTConfig) error {
	initUART(uart)

	if cfg.BaudRate == 0 {
		cfg.BaudRate = 115200
	}
	uart.baud = cfg.BaudRate

	if cfg.TX == machine.NoPin && cfg.RX == machine.NoPin {
		cfg.TX = machine.UART_TX_PIN
		cfg.RX = machine.UART_RX_PIN
	}

	// 1) Disable UART while configuring (PL011 CR).
	uart.Bus.UARTCR.ClearBits(rp.UART0_UARTCR_UARTEN | rp.UART0_UARTCR_RXE | rp.UART0_UARTCR_TXE)

	// 2) Mux pins before touching baud/format.
	if cfg.TX != machine.NoPin {
		cfg.TX.Configure(machine.PinConfig{Mode: machine.PinUART})
	}
	if cfg.RX != machine.NoPin {
		cfg.RX.Configure(machine.PinConfig{Mode: machine.PinUART})
	}
	if cfg.RTS != machine.NoPin {
		cfg.RTS.Configure(machine.PinConfig{Mode: machine.PinUART})
	}
	if cfg.CTS != machine.NoPin {
		cfg.CTS.Configure(machine.PinConfig{Mode: machine.PinUART})
	}

	// 3) Baud and format. SetFormat does a full LCR_H write including FEN.
	uart.SetBaudRate(cfg.BaudRate)       // includes the “dummy” LCR_H write required by PL011
	_ = uart.SetFormat(8, 1, ParityNone) // default 8N1 with FIFOs enabled

	// 4) Clear any pending IRQs and purge RX FIFO (read until RXFE).
	uart.Bus.UARTICR.Set(0x7FF) // clear all PL011 interrupts
	for !uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_RXFE) {
		_ = uart.Bus.UARTDR.Get()
	}
	// Clear sticky RX errors (ECR share-address via RSR).
	uart.Bus.UARTRSR.Set(0)

	// 5) Enable UART and optional flow control (only if both pins valid).
	settings := uint32(rp.UART0_UARTCR_UARTEN | rp.UART0_UARTCR_RXE | rp.UART0_UARTCR_TXE)
	if cfg.RTS != machine.NoPin && cfg.CTS != machine.NoPin {
		settings |= rp.UART0_UARTCR_RTSEN | rp.UART0_UARTCR_CTSEN
	}
	uart.Bus.UARTCR.Set(settings)

	// 6) IRQ configuration: leave TXIM masked; enable RXIM and RTIM.
	uart.Interrupt.SetPriority(0x80)
	uart.Interrupt.Enable()
	// IFLS=0 sets RX/TX thresholds to 1/8. That minimises latency and increases
	// IRQ rate. Consider RX=1/2, TX=1/8 if you prefer fewer IRQs during RX bursts.
	uart.Bus.UARTIFLS.Set(0)
	uart.Bus.UARTIMSC.Set(rp.UART0_UARTIMSC_RXIM | rp.UART0_UARTIMSC_RTIM) // TXIM is enabled by attemptSend

	// 7) Prime initial “writable” notification (FIFO starts empty).
	select {
	case uart.txNotify <- struct{}{}:
	default:
	}

	return nil
}

// SetBaudRate programs the PL011 integer and fractional divisors and performs
// the “dummy” LCR_H write required to latch them.
func (uart *UART) SetBaudRate(br uint32) {
	uart.baud = br
	div := 8 * machine.CPUFrequency() / br

	ibrd := div >> 7
	var fbrd uint32
	switch {
	case ibrd == 0:
		ibrd = 1
		fbrd = 0
	case ibrd >= 65535:
		ibrd = 65535
		fbrd = 0
	default:
		fbrd = ((div & 0x7f) + 1) / 2
	}

	uart.Bus.UARTIBRD.Set(ibrd)
	uart.Bus.UARTFBRD.Set(fbrd)

	// PL011 requires an LCR_H write after changing divisors.
	uart.Bus.UARTLCR_H.Set(uart.Bus.UARTLCR_H.Get())
}

// SetFormat sets data bits, stop bits and parity, and enables the FIFOs.
// It writes the full LCR_H value (not OR-ing).
func (uart *UART) SetFormat(databits, stopbits uint8, parity UARTParity) error {
	if databits < 5 || databits > 8 {
		return errors.New("invalid databits")
	}
	if stopbits != 1 && stopbits != 2 {
		return errors.New("invalid stopbits")
	}

	var pen, pev uint32
	if parity != ParityNone {
		pen = rp.UART0_UARTLCR_H_PEN
		if parity == ParityEven {
			pev = rp.UART0_UARTLCR_H_EPS
		}
	}
	const fen = rp.UART0_UARTLCR_H_FEN

	val := uint32((databits-5)<<rp.UART0_UARTLCR_H_WLEN_Pos|
		(stopbits-1)<<rp.UART0_UARTLCR_H_STP2_Pos) |
		pen | pev | fen

	uart.Bus.UARTLCR_H.Set(val)
	return nil
}

// RXDropCount reports bytes dropped because the software RX ring was full.
func (uart *UART) RXDropCount() uint32 { return uart.rxDrops.Get() }

// RXOverrunCount reports PL011 RX overrun error bytes seen by the ISR.
func (uart *UART) RXOverrunCount() uint32 { return uart.rxOverruns.Get() }

// RXBreakCount reports PL011 RX break error bytes seen by the ISR.
func (uart *UART) RXBreakCount() uint32 { return uart.rxBreaks.Get() }

// RXParityCount reports PL011 RX parity error bytes seen by the ISR.
func (uart *UART) RXParityCount() uint32 { return uart.rxParity.Get() }

// RXFramingCount reports PL011 RX framing error bytes seen by the ISR.
func (uart *UART) RXFramingCount() uint32 { return uart.rxFraming.Get() }

// initUART asserts and releases the peripheral reset for the selected PL011.
func initUART(uart *UART) {
	var resetVal uint32
	switch {
	case uart.Bus == rp.UART0:
		resetVal = rp.RESETS_RESET_UART0
	case uart.Bus == rp.UART1:
		resetVal = rp.RESETS_RESET_UART1
	}

	rp.RESETS.RESET.SetBits(resetVal)
	rp.RESETS.RESET.ClearBits(resetVal)
	for !rp.RESETS.RESET_DONE.HasBits(resetVal) {
	}
}

// --- TX helpers ---

// attemptSend accepts up to len(p) bytes without blocking and returns
// the number accepted into the software TX ring. It then performs a
// safe "kick" by moving bytes from the SW ring into the HW FIFO in the
// foreground only when we own TX start (TXIM masked) or in the masked
// kick corner case. Steady-state draining remains ISR-driven.
func (uart *UART) attemptSend(p []byte) int {
	if len(p) == 0 {
		return 0
	}

	// 1) Enqueue into software TX ring only.
	n := uart.enqueueTX(p)

	// Nothing queued? Still ensure TXIM is armed for any prior buffered data.
	if uart.TxBuffer.Used() == 0 {
		uart.Bus.UARTIMSC.SetBits(rp.UART0_UARTIMSC_TXIM)
		return n
	}

	// 2) Perform a safe kick from SW->HW when required, sourcing bytes from the SW ring.

	// Case A: TXIM masked — we own TX start; seed FIFO directly from SW ring.
	if !uart.Bus.UARTIMSC.HasBits(rp.UART0_UARTIMSC_TXIM) {
		for !uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_TXFF) {
			b, ok := uart.TxBuffer.Get()
			if !ok {
				break
			}
			uart.Bus.UARTDR.Set(uint32(b))
		}
		// Arm TX interrupts; ISR takes over steady-state.
		uart.Bus.UARTIMSC.SetBits(rp.UART0_UARTIMSC_TXIM)
		return n
	}

	// Case B: only kick if we can actually write ≥1 byte now.
	if uart.Bus.UARTIMSC.HasBits(rp.UART0_UARTIMSC_TXIM) &&
		!uart.Bus.UARTMIS.HasBits(rp.UART0_UARTMIS_TXMIS) &&
		!uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_TXFF) && // FIFO has space
		(uart.TxBuffer.Used() > 0) { // something to seed

		// Prevent ISR interleave while we start TX.
		uart.Bus.UARTIMSC.ClearBits(rp.UART0_UARTIMSC_TXIM)

		// Seed from SW ring until FIFO becomes full or SW empties.
		for !uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_TXFF) {
			b, ok := uart.TxBuffer.Get()
			if !ok {
				break
			}
			uart.Bus.UARTDR.Set(uint32(b))
		}

		// Re-enable TX level interrupts. Next drop to/under IFLS will raise TX IRQ.
		uart.Bus.UARTIMSC.SetBits(rp.UART0_UARTIMSC_TXIM)

		// Could perform a userland nudge here - better for latency but untested.
		// select { case uart.txNotify <- struct{}{}: default: }
	}

	return n
}

// enableTxIRQ ensures TX level interrupts are unmasked.
func (uart *UART) enableTxIRQ() {
	uart.Bus.UARTIMSC.SetBits(rp.UART0_UARTIMSC_TXIM)
}

// txFifoEmpty reports TXFE (FIFO empty). Note: PL011 does not raise an
// interrupt for TXFE alone; callers must poll if they require the exact edge.
func (uart *UART) txFifoEmpty() bool {
	return uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_TXFE)
}

// txLineIdle reports FR.BUSY==0 (shifter idle). This bit also does not raise
// an interrupt; it is only polled where precise “all bits out” is required.
func (uart *UART) txLineIdle() bool {
	return !uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_BUSY)
}

// tryWriteHW opportunistically pushes into the HW FIFO until TXFF.
func (uart *UART) tryWriteHW(p []byte) int {
	i := 0
	for i < len(p) && !uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_TXFF) {
		uart.Bus.UARTDR.Set(uint32(p[i]))
		i++
	}
	return i
}

// enqueueTX inserts into the software TX ring until full.
func (uart *UART) enqueueTX(p []byte) int {
	i := 0
	for i < len(p) {
		if ok := uart.TxBuffer.Put(p[i]); !ok {
			break
		}
		i++
	}
	return i
}

// --- RX/TX ISR ---

// handleInterrupt services RX level/timeout and TX level interrupts.
// RX: drain DR until RXFE, dropping errored bytes (read clears per-byte flags),
//
//	clear RXIC/RTIC, clear sticky errors (RSR/ECR), coalesce a Readable() wake.
//
// TX: while !TXFF, move SW→HW; coalesce a Writable() wake; when SW empty and TXFE,
//
//	coalesce a final “drained” wake and mask TXIM; clear TXIC.
func (uart *UART) handleInterrupt(interrupt.Interrupt) {
	mis := uart.Bus.UARTMIS.Get()

	// RX path (RX level or RX timeout).
	if (mis & (rp.UART0_UARTMIS_RXMIS | rp.UART0_UARTMIS_RTMIS)) != 0 {

		// In the ISR, only notify if at least one byte was enqueued.
		enq := 0
		for !uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_RXFE) {
			r := uart.Bus.UARTDR.Get()
			errs := r & (rp.UART0_UARTDR_OE | rp.UART0_UARTDR_BE |
				rp.UART0_UARTDR_PE | rp.UART0_UARTDR_FE)
			if errs != 0 {
				uart.noteRXErrors(errs)
				// Drop errored byte; reading DR clears the per-byte error flags.
				continue
			}
			if uart.Buffer.Put(byte(r & 0xFF)) {
				enq++
			} else {
				incrementRegister32(&uart.rxDrops)
			}
		}

		// Clear RX level and RX timeout sources, then sticky errors.
		uart.Bus.UARTICR.Set(rp.UART0_UARTICR_RXIC | rp.UART0_UARTICR_RTIC)
		uart.Bus.UARTRSR.Set(0)

		// Coalesce a Readable notification.
		if enq > 0 {
			select {
			case uart.notify <- struct{}{}:
			default:
			}
		}
	}

	// TX path (TX level).
	if mis&rp.UART0_UARTMIS_TXMIS != 0 {

		// Move bytes from SW buffer to HW FIFO.
		for !uart.Bus.UARTFR.HasBits(rp.UART0_UARTFR_TXFF) {
			b, ok := uart.TxBuffer.Get()
			if !ok {
				break
			}
			uart.Bus.UARTDR.Set(uint32(b))
		}

		// Coalesce a Writable notification (space/progress).
		select {
		case uart.txNotify <- struct{}{}:
		default:
		}

		// If SW buffer empty, manage the tail.
		if uart.TxBuffer.Used() == 0 {
			// When FIFO is now empty, emit a final “drained” notification and mask TXIM.
			if uart.txFifoEmpty() {
				select {
				case uart.txNotify <- struct{}{}:
				default:
				}
				uart.Bus.UARTIMSC.ClearBits(rp.UART0_UARTIMSC_TXIM)
			}
		}

		// Clear TX interrupt.
		uart.Bus.UARTICR.Set(rp.UART0_UARTICR_TXIC)
	}
}

func (uart *UART) noteRXErrors(errs uint32) {
	if errs&rp.UART0_UARTDR_OE != 0 {
		incrementRegister32(&uart.rxOverruns)
	}
	if errs&rp.UART0_UARTDR_BE != 0 {
		incrementRegister32(&uart.rxBreaks)
	}
	if errs&rp.UART0_UARTDR_PE != 0 {
		incrementRegister32(&uart.rxParity)
	}
	if errs&rp.UART0_UARTDR_FE != 0 {
		incrementRegister32(&uart.rxFraming)
	}
}

func incrementRegister32(r *volatile.Register32) {
	r.Set(r.Get() + 1)
}

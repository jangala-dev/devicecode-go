// uartx/uartx.go

//go:build atmega || esp || nrf || sam || sifive || stm32 || k210 || nxp || rp2040 || rp2350

// Package uartx provides an interrupt-driven UART driver with blocking io.Reader/io.Writer
// semantics and explicit non-blocking and flush operations. Write blocks until data is
// accepted by the driver (software TX buffer and/or hardware FIFO). Flush provides an
// explicit “on the wire” completion.
package uartx

import (
	"errors"
	"time"
)

// Flusher is implemented by types that can flush buffered output to the underlying device.
type Flusher interface{ Flush() error }

var errUARTBufferEmpty = errors.New("UART buffer empty")

// UARTParity defines the parity setting used for UART communication.
type UARTParity uint8

const (
	// ParityNone disables parity generation and checking (the most common setting).
	ParityNone UARTParity = iota
	// ParityEven sets even parity (total number of 1 bits is even).
	ParityEven
	// ParityOdd sets odd parity (total number of 1 bits is odd).
	ParityOdd
)

// Readable returns a coalesced notification for RX readiness.
// A receive interrupt that enqueues one or more bytes will send on this channel.
// The channel is level-coalesced; callers must re-check state after waking.
func (uart *UART) Readable() <-chan struct{} { return uart.notify }

// Writable returns a coalesced notification for TX progress or space.
// The driver sends on this channel when it moves bytes from software to hardware
// or when space becomes available. The channel is level-coalesced; callers must
// re-check state after waking.
func (uart *UART) Writable() <-chan struct{} { return uart.txNotify }

// TryRead returns immediately with up to len(p) bytes copied from the RX buffer.
// It never blocks and never returns an error. A return value of 0 means “no data now”.
func (uart *UART) TryRead(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	n := 0
	// Read at most len(p) bytes; stop on first empty.
	for n < len(p) {
		b, err := uart.ReadByte()
		if err != nil {
			break
		}
		p[n] = b
		n++
	}
	return n
}

// Read implements io.Reader. It blocks until at least one byte is available,
// then returns n>0, nil. It does not return io.EOF for an idle UART.
func (uart *UART) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Fast path: anything already buffered?
	if n := uart.TryRead(p); n > 0 {
		return n, nil
	}
	// Block until notified, then retry.
	for {
		select {
		case <-uart.Readable(): // coalesced wake-up; must re-check
			if n := uart.TryRead(p); n > 0 {
				return n, nil
			}
			// Spurious or lost race; loop back to wait.
		}
	}
}

// ReadByte reads a single byte from the software RX buffer.
// If there is no data available, it returns errUARTBufferEmpty.
func (uart *UART) ReadByte() (byte, error) {
	buf, ok := uart.Buffer.Get()
	if !ok {
		return 0, errUARTBufferEmpty
	}
	return buf, nil
}

// TryWrite returns immediately with 0..len(p) bytes accepted into the hardware FIFO
// and/or the software TX buffer. It never blocks and never returns an error.
// A return value of 0 means “no space now”.
func (uart *UART) TryWrite(p []byte) int {
	return uart.attemptSend(p) // non-blocking enqueue path (defined elsewhere)
}

// WriteByte writes a single byte. It blocks until the byte is accepted by the driver
// (queued to software TX and/or hardware FIFO). It does not wait for the UART to drain.
func (uart *UART) WriteByte(c byte) error {
	_, err := uart.Write([]byte{c})
	return err
}

// Writev writes the provided buffers in sequence with the same blocking behaviour as Write.
// It stops on the first error and returns the total number of bytes accepted up to that point.
func (u *UART) Writev(bufs ...[]byte) (int, error) {
	sent := 0
	for _, p := range bufs {
		n, err := u.Write(p)
		sent += n
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// Write implements io.Writer. It blocks until all bytes in p have been accepted by
// the driver (queued to software TX and/or hardware FIFO). Write does not wait for
// the UART to drain; use Flush for on-the-wire completion.
func (uart *UART) Write(p []byte) (int, error) {
	sent := 0
	for sent < len(p) {
		n := uart.TryWrite(p[sent:])
		if n > 0 {
			sent += n
			continue
		}
		// Wait for TX progress (space created or drain) then retry.
		<-uart.txNotify
	}
	return sent, nil
}

// Flush blocks until all queued bytes have left the PL011: the software TX buffer is empty,
// the TX FIFO is empty, and the line is not busy. Because PL011 does not interrupt on BUSY
// deassertion, Flush uses a short timed poll in addition to txNotify wakes.
func (uart *UART) Flush() error {
	// A small polling interval. 40–60 µs works well across common bauds.
	tick := uart.drainTick()
	for {
		if uart.TxBuffer.Used() == 0 && uart.txFifoEmpty() && uart.txLineIdle() {
			return nil
		}
		// Wake promptly if the ISR made progress; otherwise fall back to a short tick.
		select {
		case <-uart.txNotify:
			// Progress likely occurred; loop and re-check.
		case <-time.After(tick):
		}
	}
}

// drainTick returns a short polling interval for Flush based on the configured baud.
// The value is approximately two character times at 8N1, with a lower bound to avoid zero.
func (uart *UART) drainTick() time.Duration {
	if uart.baud == 0 {
		return 50 * time.Microsecond
	}
	// ~2 character times at 8N1 (10 bits/char).
	perBit := time.Second / time.Duration(uart.baud)
	t := 2 * 10 * perBit
	if t < 20*time.Microsecond {
		t = 20 * time.Microsecond
	}
	return t
}

// Buffered returns the number of bytes currently stored in the software RX buffer.
func (uart *UART) Buffered() int {
	return int(uart.Buffer.Used())
}

// TxFree returns the remaining space in the software TX buffer in bytes.
func (u *UART) TxFree() int { return int(u.TxBuffer.Size() - u.TxBuffer.Used()) }

// Receive inserts one byte into the software RX buffer. It is intended to be called
// by the UART interrupt handler when a new byte has been read from the hardware FIFO.
func (uart *UART) Receive(data byte) {
	uart.Buffer.Put(data)
}

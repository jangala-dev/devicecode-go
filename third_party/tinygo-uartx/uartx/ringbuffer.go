// uartx/ringbuffer.go

//go:build atmega || esp || nrf || sam || sifive || stm32 || k210 || nxp || rp2040 || rp2350

// An API-compatible replacement for machine.RingBuffer with an added Size() method.
// Methods and semantics match TinyGo's implementation.

package uartx

import "runtime/volatile"

// Choose a power-of-two size for efficient modulo.
const bufferSize uint8 = 128

// RingBuffer is a byte ring buffer compatible with TinyGo's machine.RingBuffer.
type RingBuffer struct {
	rxbuffer [bufferSize]volatile.Register8
	head     volatile.Register8
	tail     volatile.Register8
}

// NewRingBuffer returns a new ring buffer.
func NewRingBuffer() *RingBuffer {
	return &RingBuffer{}
}

// Size returns the total capacity of the buffer in bytes.
func (rb *RingBuffer) Size() uint8 {
	return bufferSize
}

// Used returns how many bytes in buffer have been used.
func (rb *RingBuffer) Used() uint8 {
	return uint8(rb.head.Get() - rb.tail.Get())
}

// Put stores a byte in the buffer. If the buffer is already full, it returns false.
func (rb *RingBuffer) Put(val byte) bool {
	if rb.Used() == bufferSize { // full
		return false
	}
	h := rb.head.Get()
	rb.rxbuffer[(h+1)%bufferSize].Set(val) // 1) write data
	rb.head.Set(h + 1)                     // 2) publish
	return true
}

// Get returns a byte from the buffer. If the buffer is empty, it returns (0, false).
func (rb *RingBuffer) Get() (byte, bool) {
	if rb.Used() == 0 {
		return 0, false
	}
	t := rb.tail.Get()
	v := rb.rxbuffer[(t+1)%bufferSize].Get() // 1) read current element
	rb.tail.Set(t + 1)                       // 2) publish consumption
	return v, true
}

// Clear resets the head and tail pointers to zero.
func (rb *RingBuffer) Clear() {
	rb.head.Set(0)
	rb.tail.Set(0)
}

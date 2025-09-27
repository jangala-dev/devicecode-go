package ramp

import (
	"devicecode-go/x/mathx"
	"time"
)

// Step sets the new logical level in [0..top].
type Step func(level uint16)

// Tick waits for d and reports whether to continue (false => cancelled).
type Tick func(d time.Duration) bool

// StartLinear starts a synchronous (caller-driven) integer ramp.
// Call it from a goroutine and provide Tick to handle timing & cancellation.
// steps==0 or durationMs==0 snaps to 'to'.
func StartLinear(cur, to, top uint16, durationMs uint32, steps uint16, tick Tick, set Step) {
	if steps == 0 || durationMs == 0 {
		set(mathx.Min(to, top))
		return
	}
	d := int32(int32(to) - int32(cur))
	st := int32(steps)
	acc := int32(0)
	cur32 := int32(cur)
	stepDurMs := durationMs / uint32(steps)
	if stepDurMs == 0 {
		stepDurMs = 1
	}
	stepDur := time.Duration(stepDurMs) * time.Millisecond

	for i := uint16(1); i < steps; i++ {
		if !tick(stepDur) {
			return
		}
		acc += d
		inc := acc / st
		if inc != 0 {
			acc -= inc * st
			cur32 = mathx.Clamp(cur32+inc, 0, int32(top))
			set(uint16(cur32))
		}
	}
	set(mathx.Min(to, top))
}

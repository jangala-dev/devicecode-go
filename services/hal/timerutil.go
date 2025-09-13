// services/hal/timerutil.go
package hal

import "time"

// resetTimer safely stops, drains, and resets a timer.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		drainTimer(t)
	}
	if d < 0 {
		d = 0
	}
	t.Reset(d)
}

func drainTimer(t *time.Timer) {
	select {
	case <-t.C:
	default:
	}
}

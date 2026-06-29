package fabric

import (
	"testing"
	"time"
)

func TestResetTimerDrainsExpiredTick(t *testing.T) {
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()

	<-timer.C
	resetTimer(timer, 40*time.Millisecond)

	select {
	case <-timer.C:
		t.Fatal("timer fired from a stale tick")
	case <-time.After(15 * time.Millisecond):
	}
}

//go:build !qa_reactor

package reactor

import (
	"testing"
	"time"
)

func TestWaitFabricDoneNil(t *testing.T) {
	if !waitFabricDone(nil, time.Millisecond) {
		t.Fatal("nil fabric done channel should be treated as stopped")
	}
}

func TestWaitFabricDoneClosed(t *testing.T) {
	done := make(chan struct{})
	close(done)

	if !waitFabricDone(done, 50*time.Millisecond) {
		t.Fatal("closed fabric done channel should report stopped")
	}
}

func TestWaitFabricDoneTimeout(t *testing.T) {
	done := make(chan struct{})
	start := time.Now()

	if waitFabricDone(done, 10*time.Millisecond) {
		t.Fatal("open fabric done channel should time out")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("timeout wait took too long: %s", elapsed)
	}
}

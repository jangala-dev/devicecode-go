//go:build !qa_reactor

package reactor

import (
	"context"
	"testing"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/updater"
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

func TestNewReactorDefaultsBootBuyRCZero(t *testing.T) {
	r := NewReactor(nil, nil)
	if r.bootBuyRC != 0 {
		t.Fatalf("bootBuyRC = %d, want 0", r.bootBuyRC)
	}
}

func TestNewReactorWithOptionsStoresBootBuyRC(t *testing.T) {
	r := NewReactorWithOptions(nil, nil, Options{BootBuyRC: -42})
	if r.bootBuyRC != -42 {
		t.Fatalf("bootBuyRC = %d, want -42", r.bootBuyRC)
	}
}

func TestWaitForUpdaterCriticalFactsRequiresAllThreeFacts(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	waitConn := b.NewConnection("wait")
	pubConn := b.NewConnection("pub")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan bool, 1)
	go func() {
		done <- waitForUpdaterCriticalFacts(ctx, waitConn)
	}()

	pubConn.Publish(pubConn.NewMessage(
		updater.TopicSoftwareFact,
		updater.SoftwareFact{ImageID: "img", Version: "1.0", BootID: "boot"},
		true,
	))
	pubConn.Publish(pubConn.NewMessage(
		updater.TopicUpdaterFact,
		updater.UpdaterFact{State: updater.StateRunning},
		true,
	))
	select {
	case got := <-done:
		t.Fatalf("wait returned %t before health fact", got)
	case <-time.After(20 * time.Millisecond):
	}

	pubConn.Publish(pubConn.NewMessage(
		updater.TopicHealthFact,
		updater.HealthFact{State: "ok"},
		true,
	))
	select {
	case got := <-done:
		if !got {
			t.Fatal("wait returned false after all critical facts")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for critical facts")
	}
}

func TestWaitForUpdaterCriticalFactsStopsOnContextCancel(t *testing.T) {
	b := bus.NewBus(16, "+", "#")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if waitForUpdaterCriticalFacts(ctx, b.NewConnection("wait")) {
		t.Fatal("wait returned true after context cancellation")
	}
}

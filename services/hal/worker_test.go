package hal

import (
	"context"
	"testing"
	"time"
)

// fakeAdaptor implements the generic Adaptor interface.
// It returns ErrNotReady for the first `collectsTill` Collect() calls, then succeeds.
type fakeAdaptor struct {
	id           string
	after        time.Duration
	collectsTill int // number of ErrNotReady before success
	triggers     int
	collects     int
}

func (f *fakeAdaptor) ID() string              { return f.id }
func (f *fakeAdaptor) Capabilities() []CapInfo { return nil }
func (f *fakeAdaptor) Trigger(ctx context.Context) (time.Duration, error) {
	f.triggers++
	return f.after, nil
}
func (f *fakeAdaptor) Collect(ctx context.Context) (Sample, error) {
	f.collects++
	if f.collects <= f.collectsTill {
		return nil, ErrNotReady
	}
	ts := time.Now().UnixMilli()
	return Sample{
		{Kind: "temperature", Payload: map[string]any{"deci_c": 250, "ts_ms": ts}, TsMs: ts},
		{Kind: "humidity", Payload: map[string]any{"deci_percent": 550, "ts_ms": ts}, TsMs: ts},
	}, nil
}
func (f *fakeAdaptor) Control(kind, method string, payload any) (any, error) {
	return nil, ErrUnsupported
}

func TestWorker_SuccessWithRetries(t *testing.T) {
	cfg := WorkerConfig{
		TriggerTimeout: 50 * time.Millisecond,
		CollectTimeout: 50 * time.Millisecond,
		RetryBackoff:   2 * time.Millisecond,
		MaxRetries:     5,
	}
	w := NewWorker(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	ad := &fakeAdaptor{id: "dev1", after: 1 * time.Millisecond, collectsTill: 2}
	if ok := w.Submit(MeasureReq{ID: ad.id, Adaptor: ad}); !ok {
		t.Fatal("submit failed")
	}

	select {
	case r := <-w.Results():
		if r.Err != nil {
			t.Fatalf("unexpected error: %v", r.Err)
		}
		temp := findReadingPayload(t, r.Sample, "temperature")
		hum := findReadingPayload(t, r.Sample, "humidity")
		if gi(temp, "deci_c") != 250 || gi(hum, "deci_percent") != 550 {
			t.Fatalf("bad data: temp=%v hum=%v", temp, hum)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timeout waiting for result")
	}
}

func TestWorker_RetryLimitFailure(t *testing.T) {
	cfg := WorkerConfig{RetryBackoff: 1 * time.Millisecond, MaxRetries: 2}
	w := NewWorker(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	ad := &fakeAdaptor{id: "dev2", after: 1 * time.Millisecond, collectsTill: 10}
	if ok := w.Submit(MeasureReq{ID: ad.id, Adaptor: ad}); !ok {
		t.Fatal("submit failed")
	}

	select {
	case r := <-w.Results():
		if r.Err == nil {
			t.Fatal("expected error after exhausting retries, got nil")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for failure result")
	}
}

func TestWorker_CoalescingAndReadNowDesire(t *testing.T) {
	cfg := WorkerConfig{
		RetryBackoff: 1 * time.Millisecond,
		MaxRetries:   1, // force a quick collect failure
	}
	w := NewWorker(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Submit a job that will fail its first collect cycle (ErrNotReady twice).
	ad := &fakeAdaptor{id: "dev3", after: 1 * time.Millisecond, collectsTill: 2}

	if ok := w.Submit(MeasureReq{ID: ad.id, Adaptor: ad}); !ok {
		t.Fatal("submit failed")
	}
	// While pending, submit a priority request to set the desire flag.
	_ = w.Submit(MeasureReq{ID: ad.id, Adaptor: ad, Prio: true})

	// First result should be an error (due to retries exhausted).
	select {
	case r := <-w.Results():
		if r.Err == nil {
			t.Fatal("expected error on first cycle")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timeout waiting for first failure")
	}

	// Make subsequent collect succeed.
	ad.collectsTill = 0

	// Expect success from the immediate re-trigger driven by desire.
	select {
	case r := <-w.Results():
		if r.Err != nil {
			t.Fatalf("unexpected second error: %v", r.Err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timeout waiting for success after desire re-trigger")
	}
	if ad.triggers < 2 {
		t.Fatalf("expected at least 2 triggers, got %d", ad.triggers)
	}
}

// -------- helpers --------

func findReadingPayload(t *testing.T, s Sample, kind string) map[string]any {
	t.Helper()
	for _, r := range s {
		if r.Kind == kind {
			if m, ok := r.Payload.(map[string]any); ok {
				return m
			}
			t.Fatalf("payload for kind %q is not a map: %#v", kind, r.Payload)
		}
	}
	t.Fatalf("reading kind %q not found in sample: %#v", kind, s)
	return nil
}

func gi(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float32:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

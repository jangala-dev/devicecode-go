package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"devicecode-go/services/hal/internal/halcore"
)

type fakeAdaptor struct {
	id          string
	delay       time.Duration
	collectErrs int // number of consecutive ErrNotReady before success
	failErr     error
}

func (f *fakeAdaptor) ID() string                      { return f.id }
func (f *fakeAdaptor) Capabilities() []halcore.CapInfo { return nil }
func (f *fakeAdaptor) Trigger(ctx context.Context) (time.Duration, error) {
	if f.failErr != nil {
		return 0, f.failErr
	}
	return f.delay, nil
}
func (f *fakeAdaptor) Collect(ctx context.Context) (halcore.Sample, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	if f.collectErrs > 0 {
		f.collectErrs--
		return nil, halcore.ErrNotReady
	}
	return halcore.Sample{{Kind: "temp", Payload: 123, TsMs: time.Now().UnixMilli()}}, nil
}
func (f *fakeAdaptor) Control(string, string, any) (any, error) { return nil, halcore.ErrUnsupported }

func TestMeasureWorkerSuccessWithRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan halcore.Result, 1)
	w := New(halcore.WorkerConfig{
		TriggerTimeout: 5 * time.Millisecond,
		CollectTimeout: 10 * time.Millisecond,
		RetryBackoff:   2 * time.Millisecond,
		MaxRetries:     5,
		InputQueueSize: 4,
	}, results)
	w.Start(ctx)

	ad := &fakeAdaptor{id: "dev1", delay: 1 * time.Millisecond, collectErrs: 2}
	if !w.Submit(halcore.MeasureReq{ID: ad.id, Adaptor: ad}) {
		t.Fatal("submit failed")
	}

	select {
	case r := <-results:
		if r.Err != nil || len(r.Sample) == 0 {
			t.Fatalf("unexpected result: %+v", r)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for result")
	}
}

func TestMeasureWorkerErrorPathAndPrio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan halcore.Result, 2)
	w := New(halcore.WorkerConfig{}, results)
	w.Start(ctx)

	ad := &fakeAdaptor{id: "devX", delay: 1 * time.Millisecond, failErr: errors.New("boom")}
	// Submit normal request that will error.
	if !w.Submit(halcore.MeasureReq{ID: ad.id, Adaptor: ad}) {
		t.Fatal("submit failed")
	}
	// While failing, queue a prio request; worker should honour by re-triggering immediately after error path.
	if !w.Submit(halcore.MeasureReq{ID: ad.id, Adaptor: ad, Prio: true}) {
		t.Fatal("prio submit failed")
	}

	// Expect at least one error result.
	select {
	case r := <-results:
		if r.Err == nil {
			t.Fatalf("expected error result, got %+v", r)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for error result")
	}
}

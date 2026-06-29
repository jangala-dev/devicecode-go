package updater

import (
	"io"
	"strings"
	"testing"
	"time"
)

type actorBlockingVerifier struct {
	entered  chan struct{}
	release  chan struct{}
	manifest Manifest
}

func (v *actorBlockingVerifier) Verify(r io.Reader, sink SlotSink) (Manifest, error) {
	select {
	case <-v.entered:
	default:
		close(v.entered)
	}
	<-v.release
	if _, err := io.Copy(sink, r); err != nil {
		return Manifest{}, err
	}
	if err := sink.Commit(); err != nil {
		return Manifest{}, err
	}
	return v.manifest, nil
}

func TestStreamedStageActorRejectsConcurrentCommandWhileWorkerBusy(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	verif := &actorBlockingVerifier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		manifest: Manifest{
			Version:       "9.9.9",
			BuildID:       "build-9.9.9",
			ImageID:       "mcu-dev-9.9.9",
			PayloadSHA256: strings.Repeat("a", 64),
			PayloadLength: 4,
		},
	}

	svc, cancel := runService(t, b, Options{Conn: conn, Verifier: verif})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-actor-busy", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if err := svc.WriteStreamedStage("xfer-actor-busy", gen, []byte("blob")); err != nil {
		t.Fatalf("WriteStreamedStage: %v", err)
	}

	commitErr := make(chan error, 1)
	go func() {
		_, err := svc.CommitStreamedStage("xfer-actor-busy", gen)
		commitErr <- err
	}()
	select {
	case <-verif.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("verifier did not enter")
	}

	if err := svc.WriteStreamedStage("xfer-actor-busy", gen, []byte("more")); err == nil || err.Error() != ErrBusy {
		t.Fatalf("WriteStreamedStage while commit pending err = %v, want busy", err)
	}

	close(verif.release)
	select {
	case err := <-commitErr:
		if err != nil {
			t.Fatalf("CommitStreamedStage after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("commit did not complete")
	}
}

func TestStreamedStageActorCancelWhileWorkerBusyRejectsLateWorkerSuccess(t *testing.T) {
	b := newTestBus()
	conn := b.NewConnection("updater")
	caller := b.NewConnection("caller")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	memMD := NewMemoryMetadata()
	verif := &actorBlockingVerifier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		manifest: Manifest{
			Version:       "9.9.9",
			BuildID:       "build-9.9.9",
			ImageID:       "mcu-dev-9.9.9",
			PayloadSHA256: strings.Repeat("b", 64),
			PayloadLength: 4,
		},
	}

	svc, cancel := runService(t, b, Options{
		Conn:          conn,
		Verifier:      verif,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancel()
	prepareUpdaterForLease(t, caller)
	gen, err := svc.BeginStreamedStage("xfer-cancel-busy", 4)
	if err != nil {
		t.Fatalf("BeginStreamedStage: %v", err)
	}
	if err := svc.WriteStreamedStage("xfer-cancel-busy", gen, []byte("blob")); err != nil {
		t.Fatalf("WriteStreamedStage: %v", err)
	}

	commitErr := make(chan error, 1)
	go func() {
		_, err := svc.CommitStreamedStage("xfer-cancel-busy", gen)
		commitErr <- err
	}()
	select {
	case <-verif.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("verifier did not enter")
	}

	svc.CancelStreamedStage("xfer-cancel-busy", gen, "outer_timeout")
	failed := waitForFact[UpdaterFact](t, upSub, func(f UpdaterFact) bool { return f.State == StateFailed })
	if got := strValue(failed.LastError); got != "outer_timeout" {
		t.Fatalf("last_error = %q, want outer_timeout", got)
	}
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("descriptor persisted before blocked verifier was released")
	}

	close(verif.release)
	select {
	case err := <-commitErr:
		if err == nil {
			t.Fatal("CommitStreamedStage succeeded after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("commit did not return after verifier release")
	}
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("late worker success persisted descriptor after cancellation")
	}
}

package fabric

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"devicecode-go/services/updater"
)

type integrationVerifier struct {
	mu       sync.Mutex
	want     []byte
	got      []byte
	manifest updater.Manifest
	err      error
}

func (v *integrationVerifier) Verify(r io.Reader, sink updater.SlotSink) (updater.Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		if sink != nil {
			_ = sink.Abort()
		}
		return updater.Manifest{}, err
	}
	v.mu.Lock()
	v.got = append([]byte(nil), data...)
	want := append([]byte(nil), v.want...)
	verr := v.err
	v.mu.Unlock()
	if verr != nil {
		if sink != nil {
			_ = sink.Abort()
		}
		return updater.Manifest{}, verr
	}
	if want != nil && !bytes.Equal(data, want) {
		if sink != nil {
			_ = sink.Abort()
		}
		return updater.Manifest{}, errors.New("artefact_bytes_mismatch")
	}
	if sink != nil {
		if _, err := sink.Write(data); err != nil {
			return updater.Manifest{}, err
		}
		if err := sink.Commit(); err != nil {
			return updater.Manifest{}, err
		}
	}
	return v.manifest, nil
}

func (v *integrationVerifier) bytesSeen() []byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]byte(nil), v.got...)
}

type integrationApplier struct {
	mu          sync.Mutex
	canCalls    []updater.StagedDescriptor
	rebootCalls []updater.StagedDescriptor
	rebootCh    chan updater.StagedDescriptor
}

func (a *integrationApplier) CanApply(d updater.StagedDescriptor) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.canCalls = append(a.canCalls, d)
	return nil
}

func (a *integrationApplier) ArmReboot(d updater.StagedDescriptor) error {
	a.mu.Lock()
	a.rebootCalls = append(a.rebootCalls, d)
	ch := a.rebootCh
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- d:
		default:
		}
	}
	return nil
}

func (a *integrationApplier) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.canCalls), len(a.rebootCalls)
}

func waitForMcuUpdateDone(t *testing.T, tr Transport, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for xfer_done id=%s", id)
		}
		line, err := tr.ReadLine()
		if err != nil {
			t.Fatalf("ReadLine: %v", err)
		}
		var probe struct {
			Type   string `json:"type"`
			XferID string `json:"xfer_id"`
			Err    string `json:"err"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Fatalf("Unmarshal %q: %v", line, err)
		}
		switch probe.Type {
		case msgXferDone:
			if probe.XferID != id {
				t.Fatalf("xfer_done id = %q, want %q", probe.XferID, id)
			}
			return
		case msgXferAbort:
			t.Fatalf("transfer aborted while waiting for done: %+v", probe)
		}
	}
}

func sendMcuUpdateArtefact(t *testing.T, tr Transport, id string, payload []byte, chunkSizes ...int) {
	t.Helper()
	sendMsg(t, tr, xferBegin(id, payload, nil))
	readTransferReady(t, tr, id, 0)
	writeRawLine(t, tr, `{"type":"unknown_noise","ignored":true}`)
	off := 0
	for len(payload[off:]) > 0 {
		n := len(payload) - off
		if len(chunkSizes) > 0 {
			n = chunkSizes[0]
			chunkSizes = chunkSizes[1:]
			if n > len(payload)-off {
				n = len(payload) - off
			}
		}
		part := payload[off : off+n]
		sendMsg(t, tr, xferChunk(id, uint32(off), part))
		off += n
		readTransferNeed(t, tr, id, uint32(off))
	}
	sendMsg(t, tr, xferCommit(id, payload))
	waitForMcuUpdateDone(t, tr, id)
}

func TestMCUUpdateFullWirePathStagesAndCommitsReboot(t *testing.T) {
	b := newBus()
	caller := b.NewConnection("caller")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(updater.TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)

	payload := []byte("signed-envelope-and-payload-for-mcu")
	manifest := updater.Manifest{
		Version:       "2.0.0",
		BuildID:       "build-2.0.0",
		ImageID:       "mcu-image-new",
		PayloadSHA256: strings.Repeat("c", 64),
		PayloadLength: uint32(len(payload)),
	}
	verif := &integrationVerifier{want: payload, manifest: manifest}
	memMD := updater.NewMemoryMetadata()
	app := &integrationApplier{rebootCh: make(chan updater.StagedDescriptor, 1)}
	cancelUpdater, updaterSvc := runUpdaterForFabricTest(t, b, updater.Options{
		Verifier:      verif,
		Applier:       app,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancelUpdater()
	prepareUpdaterForFabricTest(t, caller)

	cm5, mcu := pipePair()
	ctx, cancelFabric := context.WithCancel(context.Background())
	defer cancelFabric()
	go RunWithOptions(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig(), RunOptions{StageController: updaterSvc})
	bringUp(t, cm5)

	sendMcuUpdateArtefact(t, cm5, "xfer-full-path", payload, 7, 5)
	waitUpdaterFactForFabricTest(t, upSub, func(f updater.UpdaterFact) bool { return f.State == updater.StateStaged })
	if got := verif.bytesSeen(); !bytes.Equal(got, payload) {
		t.Fatalf("verifier saw %q, want %q", got, payload)
	}
	desc, ok := memMD.StagedDescriptor()
	if !ok {
		t.Fatal("staged descriptor not persisted")
	}
	if desc.Version != manifest.Version || desc.ImageID != manifest.ImageID || desc.PayloadSHA256 != manifest.PayloadSHA256 || desc.Length != manifest.PayloadLength {
		t.Fatalf("staged descriptor = %+v, want manifest %+v", desc, manifest)
	}

	payloadReply := requestUpdaterForFabricTest(t, caller, updater.TopicCommitRPC, updater.CommitRequest{})
	commit, ok := payloadReply.(updater.CommitReply)
	if !ok || !commit.Accepted || !commit.RebootRequired {
		t.Fatalf("commit reply = %#v, want accepted reboot_required", payloadReply)
	}
	select {
	case rebootDesc := <-app.rebootCh:
		if rebootDesc.ImageID != manifest.ImageID || rebootDesc.Version != manifest.Version {
			t.Fatalf("reboot descriptor = %+v, want image %s version %s", rebootDesc, manifest.ImageID, manifest.Version)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ArmReboot")
	}
	waitUpdaterFactForFabricTest(t, upSub, func(f updater.UpdaterFact) bool { return f.State == updater.StateRebooting })
	can, reboot := app.counts()
	if can != 1 || reboot != 1 {
		t.Fatalf("applier calls: CanApply=%d ArmReboot=%d, want 1 and 1", can, reboot)
	}
}

func TestMCUUpdateFullWirePathCommitRejectsExpectedImageMismatch(t *testing.T) {
	b := newBus()
	caller := b.NewConnection("caller")
	payload := []byte("firmware-bytes")
	manifest := updater.Manifest{
		Version:       "2.1.0",
		BuildID:       "build-2.1.0",
		ImageID:       "mcu-image-real",
		PayloadSHA256: strings.Repeat("d", 64),
		PayloadLength: uint32(len(payload)),
	}
	memMD := updater.NewMemoryMetadata()
	app := &integrationApplier{rebootCh: make(chan updater.StagedDescriptor, 1)}
	cancelUpdater, updaterSvc := runUpdaterForFabricTest(t, b, updater.Options{
		Verifier:      &integrationVerifier{want: payload, manifest: manifest},
		Applier:       app,
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancelUpdater()
	prepareUpdaterForFabricTest(t, caller)

	cm5, mcu := pipePair()
	ctx, cancelFabric := context.WithCancel(context.Background())
	defer cancelFabric()
	go RunWithOptions(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig(), RunOptions{StageController: updaterSvc})
	bringUp(t, cm5)

	sendMcuUpdateArtefact(t, cm5, "xfer-mismatch", payload, 4)
	if _, ok := memMD.StagedDescriptor(); !ok {
		t.Fatal("staged descriptor not persisted before mismatch commit")
	}

	payloadReply := requestUpdaterForFabricTest(t, caller, updater.TopicCommitRPC, updater.CommitRequest{ExpectedImageID: "mcu-image-other"})
	reply, ok := payloadReply.(updater.Reply)
	if !ok || reply.OK || reply.Error != updater.ErrImageIDMismatch {
		t.Fatalf("commit mismatch reply = %#v, want image_id_mismatch", payloadReply)
	}
	can, reboot := app.counts()
	if can != 0 || reboot != 0 {
		t.Fatalf("applier called despite image mismatch: CanApply=%d ArmReboot=%d", can, reboot)
	}
	select {
	case d := <-app.rebootCh:
		t.Fatalf("unexpected reboot after mismatch: %+v", d)
	default:
	}
}

func TestMCUUpdateWireDigestMismatchCancelsLeaseAndLeavesNoStagedImage(t *testing.T) {
	b := newBus()
	caller := b.NewConnection("caller")
	observer := b.NewConnection("observer")
	upSub := observer.Subscribe(updater.TopicUpdaterFact)
	defer observer.Unsubscribe(upSub)
	memMD := updater.NewMemoryMetadata()
	cancelUpdater, updaterSvc := runUpdaterForFabricTest(t, b, updater.Options{
		Verifier:      &integrationVerifier{},
		Metadata:      memMD,
		MetadataWrite: memMD,
	})
	defer cancelUpdater()
	prepareUpdaterForFabricTest(t, caller)

	cm5, mcu := pipePair()
	ctx, cancelFabric := context.WithCancel(context.Background())
	defer cancelFabric()
	go RunWithOptions(ctx, mcu, b.NewConnection("fabric"), "mcu", "bigbox-cm5", DefaultLinkConfig(), RunOptions{StageController: updaterSvc})
	bringUp(t, cm5)

	payload := []byte("abcd")
	bogusDigest := strings.Repeat("0", 8)
	sendMsg(t, cm5, protoXferBegin{
		Type:      msgXferBegin,
		XferID:    "xfer-digest-mismatch-real",
		Target:    updater.TargetUpdaterMain,
		Size:      uint32(len(payload)),
		DigestAlg: updater.DigestAlgXXHash32,
		Digest:    bogusDigest,
	})
	readTransferReady(t, cm5, "xfer-digest-mismatch-real", 0)
	sendMsg(t, cm5, xferChunk("xfer-digest-mismatch-real", 0, payload))
	readTransferNeed(t, cm5, "xfer-digest-mismatch-real", uint32(len(payload)))
	sendMsg(t, cm5, protoXferCommit{
		Type:      msgXferCommit,
		XferID:    "xfer-digest-mismatch-real",
		Size:      uint32(len(payload)),
		DigestAlg: updater.DigestAlgXXHash32,
		Digest:    bogusDigest,
	})
	readTransferAbort(t, cm5, "xfer-digest-mismatch-real", "digest_mismatch")

	failed := waitUpdaterFactForFabricTest(t, upSub, func(f updater.UpdaterFact) bool { return f.State == updater.StateFailed })
	if got := strValueFabric(failed.LastError); got != "digest_mismatch" {
		t.Fatalf("last_error = %q, want digest_mismatch", got)
	}
	if _, ok := memMD.StagedDescriptor(); ok {
		t.Fatal("digest mismatch left a staged descriptor")
	}
	payloadReply := requestUpdaterForFabricTest(t, caller, updater.TopicCommitRPC, updater.CommitRequest{})
	reply, ok := payloadReply.(updater.Reply)
	if !ok || reply.OK || reply.Error != updater.ErrNoStagedImage {
		t.Fatalf("commit after digest mismatch = %#v, want no_staged_image", payloadReply)
	}
}

func strValueFabric(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

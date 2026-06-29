//go:build !tinygo

package main

import (
	"testing"

	"devicecode-go/services/updater"
)

func TestStateStorePersistsRunningAndStagedDescriptor(t *testing.T) {
	dir := t.TempDir()
	store, err := openStateStore(dir, imageState{ImageID: "mcu-dev-10.0", Version: "10.0", BuildID: "initial"})
	if err != nil {
		t.Fatalf("openStateStore: %v", err)
	}
	if id := store.identity(); id.ImageID != "mcu-dev-10.0" || id.Version != "10.0" {
		t.Fatalf("identity = %+v", id)
	}
	desc := updater.StagedDescriptor{ImageID: "mcu-dev-15.0", Version: "15.0", BuildID: "build-15", Length: 1234, PayloadSHA256: "abc"}
	if err := store.WriteStagedDescriptor(desc); err != nil {
		t.Fatalf("WriteStagedDescriptor: %v", err)
	}
	got, ok := store.StagedDescriptor()
	if !ok || got.ImageID != desc.ImageID {
		t.Fatalf("staged = %+v ok=%v", got, ok)
	}

	reopened, err := openStateStore(dir, imageState{ImageID: "ignored", Version: "ignored"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok = reopened.StagedDescriptor()
	if !ok || got.ImageID != desc.ImageID {
		t.Fatalf("reopened staged = %+v ok=%v", got, ok)
	}
}

func TestDevhostApplierMarksRunningAndExits(t *testing.T) {
	store, err := openStateStore(t.TempDir(), imageState{ImageID: "mcu-dev-10.0", Version: "10.0"})
	if err != nil {
		t.Fatal(err)
	}
	desc := updater.StagedDescriptor{ImageID: "mcu-dev-15.0", Version: "15.0", BuildID: "build-15", Length: 1234, PayloadSHA256: "sha"}
	var exitCode int
	applier := devhostApplier{store: store, exitCode: 42, exit: func(code int) { exitCode = code }}
	if err := applier.CanApply(desc); err != nil {
		t.Fatalf("CanApply: %v", err)
	}
	if err := applier.ArmReboot(desc); err != nil {
		t.Fatalf("ArmReboot: %v", err)
	}
	if exitCode != 42 {
		t.Fatalf("exitCode=%d", exitCode)
	}
	id := store.identity()
	if id.ImageID != "mcu-dev-15.0" || id.Version != "15.0" || id.Build != "build-15" {
		t.Fatalf("running identity = %+v", id)
	}
	if _, ok := store.StagedDescriptor(); ok {
		t.Fatalf("staged descriptor was not cleared")
	}
}

//go:build !tinygo

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"devicecode-go/services/updater"
)

type devhostApplier struct {
	store    *stateStore
	exitCode int
	exit     func(int)
}

func (a devhostApplier) CanApply(d updater.StagedDescriptor) error {
	if d.ImageID == "" {
		return errors.New("staged_image_id_required")
	}
	if d.Length == 0 {
		return errors.New("staged_length_required")
	}
	if d.PayloadSHA256 == "" {
		return errors.New("staged_payload_sha256_required")
	}
	return nil
}

func (a devhostApplier) ArmReboot(d updater.StagedDescriptor) error {
	if a.store == nil {
		return errors.New("devhost_state_store_required")
	}
	if err := a.store.MarkRunningFromStaged(d); err != nil {
		return err
	}
	logJSON(map[string]any{"event": "rebooting", "image_id": d.ImageID, "version": d.Version, "length": d.Length})
	if a.exit != nil {
		a.exit(a.exitCode)
		return nil
	}
	os.Exit(a.exitCode)
	return nil
}

func logJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Printf("{\"event\":\"log_error\",\"err\":%q}\n", err.Error())
		return
	}
	fmt.Println(string(b))
}

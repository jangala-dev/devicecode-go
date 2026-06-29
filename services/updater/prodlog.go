package updater

import (
	"devicecode-go/utilities/diag"
	"devicecode-go/x/strconvx"
)

func logUpdaterPrepareResult(ok bool, reason string, generation uint64) {
	diag.Println("[updater-stream]", "ev", "prepare", "ok", ok, "reason", reason, "generation", strconvx.Itoa64(int64(generation)))
}

func logUpdaterStageResult(xferID string, ok bool, err string, generation uint64, size uint32) {
	diag.Println("[updater-stream]", "ev", "stage_reply", "xfer_id", xferID, "ok", ok, "err", err, "generation", strconvx.Itoa64(int64(generation)), "size", strconvx.Itoa(int(size)))
}

func logUpdaterCommitResult(xferID string, ok bool, err string, generation uint64, written uint32, imageID, version string) {
	diag.Println("[updater-stream]", "ev", "commit_result", "xfer_id", xferID, "ok", ok, "err", err, "generation", strconvx.Itoa64(int64(generation)), "written", strconvx.Itoa(int(written)), "image_id", imageID, "version", version)
}

func logUpdaterCommitAccepted(jobID, imageID, version string, length uint32, slot int) {
	diag.Println("[updater-commit]", "ev", "accepted", "job_id", jobID, "image_id", imageID, "version", version, "length", strconvx.Itoa(int(length)), "slot", strconvx.Itoa(slot))
}

func logUpdaterCommitReject(reason string) {
	diag.Println("[updater-commit]", "ev", "reject", "reason", reason)
}

func logUpdaterRebootArm(imageID string, slot int) {
	diag.Println("[updater-commit]", "ev", "arm_reboot_start", "image_id", imageID, "slot", strconvx.Itoa(slot))
}

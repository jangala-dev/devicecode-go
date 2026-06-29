package updater

import (
	"encoding/json"
	"testing"
)

func requireValidUpdaterJSON(t *testing.T, name string, b []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("%s invalid JSON: %v\n%s", name, err, string(b))
	}
}

func TestAppendJSONUpdaterPayloadsAreValid(t *testing.T) {
	last := "timeout"
	ver := "1.2.3"
	img := "img-2"
	job := "job-1"
	bootRC := int32(-7)
	cases := map[string][]byte{
		"PrepareReply":     PrepareReply{Ready: true, Target: PrepareTargetMCU, MaxChunkSize: 2048}.AppendJSON(nil),
		"CommitReply":      CommitReply{Accepted: true, RebootRequired: true}.AppendJSON(nil),
		"Reply":            Reply{OK: false, Error: ErrBusy}.AppendJSON(nil),
		"SoftwareFact":     SoftwareFact{Version: "1.2.3", BuildID: "abc", ImageID: "img-1", BootID: "boot", PayloadSHA256: "0123"}.AppendJSON(nil),
		"UpdaterFact":      UpdaterFact{State: StateFailed, LastError: &last, PendingVersion: &ver, PendingImageID: &img, StagedImageID: &img, JobID: &job, BootBuyRC: &bootRC}.AppendJSON(nil),
		"UpdaterFactNulls": UpdaterFact{State: StateRunning}.AppendJSON(nil),
		"HealthFact":       HealthFact{State: "degraded", Reason: "stale"}.AppendJSON(nil),
		"StageReply":       StageReply{OK: false, Err: ErrBusy, Stage: "prepare"}.AppendJSON(nil),
		"StagedDescriptor": StagedDescriptor{Version: "1.2.3", BuildID: "abc", ImageID: "img-1", Length: 42, Slot: 1, PayloadSHA256: "sha"}.AppendJSON(nil),
	}
	for name, b := range cases {
		requireValidUpdaterJSON(t, name, b)
	}
}

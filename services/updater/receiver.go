package updater

import (
	"errors"

	"devicecode-go/bus"
)

// The SlotSink used during verification is created via newSlotSink,
// which is build-tag-split: host returns a RAM buffer (sink_host.go),
// tinygo+rp2350 returns an abupdate-backed sink that streams into the
// inactive A/B slot (sink_tinygo.go).

// handleStage runs the verifier-gated staging path. Triggered by fabric
// after xfer_commit; the reply gates whether fabric sends xfer_done or
// xfer_abort.
//
// On verifier success: write staged descriptor, publish state=staged
// with the manifest's version as pending_version, return ok=true.
//
// On verifier failure: publish state=failed with the verifier's error
// string in last_error, return ok=false.
func (s *Service) handleStage(msg *bus.Message) {
	payload, ok := jsonDecode[StagePayload](msg.Payload)
	if !ok {
		s.reply(msg, StageReply{OK: false, Err: "bad_payload"})
		return
	}
	if payload.Target != TargetUpdaterMain {
		s.reply(msg, StageReply{OK: false, Err: "unsupported_target"})
		return
	}
	if payload.DigestAlg != "" && payload.DigestAlg != DigestAlgXXHash32 {
		s.reply(msg, StageReply{OK: false, Err: "unsupported_digest_alg"})
		return
	}
	if err := s.checkStreamedStageLease(payload.XferID, payload.Generation, true); err != nil {
		s.reply(msg, StageReply{OK: false, Err: err.Error()})
		return
	}

	staged, ok := s.consumeStreamedStageResult()
	if !ok {
		s.failStage(payload, "artefact_missing")
		s.reply(msg, StageReply{OK: false, Err: "artefact_missing"})
		return
	}
	if err := s.checkStreamedStageLease(payload.XferID, payload.Generation, true); err != nil {
		s.failLateStage(payload, err)
		s.reply(msg, StageReply{OK: false, Err: err.Error()})
		return
	}
	desc := StagedDescriptor{
		Version:       staged.Version,
		BuildID:       staged.BuildID,
		ImageID:       staged.ImageID,
		Length:        staged.Length,
		Slot:          0,
		PayloadSHA256: staged.PayloadSHA256,
	}
	if err := s.metadataWrite.WriteStagedDescriptor(desc); err != nil {
		s.failStage(payload, "metadata_write_failed:"+err.Error())
		s.reply(msg, StageReply{OK: false, Err: "metadata_write_failed"})
		return
	}
	if err := s.checkStreamedStageLease(payload.XferID, payload.Generation, true); err != nil {
		s.failLateStage(payload, err)
		s.reply(msg, StageReply{OK: false, Err: err.Error()})
		return
	}
	if !s.releaseStreamedStageLease(payload.XferID, payload.Generation) {
		err := errors.New("stage_cancelled")
		s.failLateStage(payload, err)
		s.reply(msg, StageReply{OK: false, Err: err.Error()})
		return
	}
	s.setStagedImage(desc.ImageID, desc.Version)
	s.transitionTo(StateStaged, "", desc.Version)
	s.reply(msg, StageReply{OK: true, Stage: "staged"})
}

func (s *Service) failStage(payload StagePayload, reason string) {
	_ = s.metadataWrite.ClearStagedDescriptor()
	s.clearStagedImage()
	if payload.Generation != 0 {
		s.cancelStreamedStageLease(payload.XferID, payload.Generation, reason)
	}
	s.transitionTo(StateFailed, reason, "")
}

func (s *Service) failLateStage(payload StagePayload, err error) {
	reason := "stage_cancelled"
	if err != nil {
		reason = err.Error()
	}
	_ = s.metadataWrite.ClearStagedDescriptor()
	s.clearStagedImage()
	if payload.Generation != 0 {
		s.cancelStreamedStageLease(payload.XferID, payload.Generation, reason)
	}
	s.transitionTo(StateFailed, reason, "")
}

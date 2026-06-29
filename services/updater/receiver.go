package updater

import "errors"

// The SlotSink used during verification is created via newSlotSink,
// which is build-tag-split: host returns a RAM buffer (sink_host.go),
// tinygo+rp2350 returns an abupdate-backed sink that streams into the
// inactive A/B slot (sink_tinygo.go).

// stage runs the verifier-gated staging path. Triggered by fabric after
// xfer_commit through the local bound endpoint; the reply gates whether fabric
// sends xfer_done or xfer_abort.
func (s *Service) stage(payloadAny any) any {
	payload, ok := jsonDecode[StagePayload](payloadAny)
	if !ok {
		return StageReply{OK: false, Err: "bad_payload"}
	}
	if payload.Target != TargetUpdaterMain {
		return StageReply{OK: false, Err: "unsupported_target"}
	}
	if payload.DigestAlg != "" && payload.DigestAlg != DigestAlgXXHash32 {
		return StageReply{OK: false, Err: "unsupported_digest_alg"}
	}
	if err := s.checkStreamedStageLease(payload.XferID, payload.Generation, true); err != nil {
		return StageReply{OK: false, Err: err.Error()}
	}

	staged, ok := s.consumeStreamedStageResult()
	if !ok {
		s.failStage(payload, "artefact_missing")
		return StageReply{OK: false, Err: "artefact_missing"}
	}
	if err := s.checkStreamedStageLease(payload.XferID, payload.Generation, true); err != nil {
		s.failLateStage(payload, err)
		return StageReply{OK: false, Err: err.Error()}
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
		return StageReply{OK: false, Err: "metadata_write_failed"}
	}
	if err := s.checkStreamedStageLease(payload.XferID, payload.Generation, true); err != nil {
		s.failLateStage(payload, err)
		return StageReply{OK: false, Err: err.Error()}
	}
	if !s.releaseStreamedStageLease(payload.XferID, payload.Generation) {
		err := errors.New("stage_cancelled")
		s.failLateStage(payload, err)
		return StageReply{OK: false, Err: err.Error()}
	}
	s.setStagedImage(desc.ImageID, desc.Version)
	s.transitionTo(StateStaged, "", desc.Version)
	return StageReply{OK: true, Stage: "staged"}
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

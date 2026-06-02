package updater

import (
	"bytes"
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

	if len(payload.Artefact) == 0 {
		staged, ok := consumeStreamedStageResult()
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
		stageIdentity, _ := identityFromStageMeta(s.identity, payload.Meta)
		if staged.Version != "" {
			stageIdentity.Version = staged.Version
		}
		if staged.BuildID != "" {
			stageIdentity.Build = staged.BuildID
		}
		if staged.ImageID != "" {
			stageIdentity.ImageID = staged.ImageID
		}
		desc := StagedDescriptor{
			Version:       stageIdentity.Version,
			BuildID:       stageIdentity.Build,
			ImageID:       stageIdentity.ImageID,
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
		return
	}

	sink, err := newSlotSink(uint32(len(payload.Artefact)))
	if err != nil {
		s.failStage(payload, "sink_init_failed:"+err.Error())
		s.reply(msg, StageReply{OK: false, Err: "sink_init_failed"})
		return
	}
	if err := s.checkStreamedStageLease(payload.XferID, payload.Generation, true); err != nil {
		_ = sink.Abort()
		s.failLateStage(payload, err)
		s.reply(msg, StageReply{OK: false, Err: err.Error()})
		return
	}
	manifest, err := s.verifier.Verify(bytes.NewReader(payload.Artefact), sink)
	if err != nil {
		// Verifier rejected the artefact. Clear any prior descriptor so a
		// following commit cannot apply stale firmware from an older stage.
		s.failStage(payload, err.Error())
		s.reply(msg, StageReply{OK: false, Err: err.Error()})
		return
	}
	if err := s.checkStreamedStageLease(payload.XferID, payload.Generation, true); err != nil {
		s.failLateStage(payload, err)
		s.reply(msg, StageReply{OK: false, Err: err.Error()})
		return
	}
	desc := StagedDescriptor{
		Version:       manifest.Version,
		BuildID:       manifest.BuildID,
		ImageID:       manifest.ImageID,
		Length:        manifest.PayloadLength,
		Slot:          0, // slot-pick comes from abupdate when hardware apply is wired
		PayloadSHA256: manifest.PayloadSHA256,
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
	s.setStagedImage(desc.ImageID, manifest.Version)
	s.transitionTo(StateStaged, "", manifest.Version)
	// Do not republish the software fact here: PayloadSHA256 describes the
	// running image, while this descriptor describes the staged image.
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

type stageMetadata struct {
	Version         string `json:"version,omitempty"`
	Build           string `json:"build,omitempty"`
	BuildID         string `json:"build_id,omitempty"`
	ImageID         string `json:"image_id,omitempty"`
	ExpectedImageID string `json:"expected_image_id,omitempty"`
}

type stageMetadataEnvelope struct {
	Metadata stageMetadata `json:"metadata,omitempty"`
	Meta     stageMetadata `json:"meta,omitempty"`
	Request  struct {
		Metadata        stageMetadata `json:"metadata,omitempty"`
		Meta            stageMetadata `json:"meta,omitempty"`
		ExpectedImageID string        `json:"expected_image_id,omitempty"`
	} `json:"request,omitempty"`
}

func applyStageMetadata(ident *Identity, md stageMetadata) bool {
	applied := false
	if md.Version != "" {
		ident.Version = md.Version
		applied = true
	}
	if md.BuildID != "" {
		ident.Build = md.BuildID
		applied = true
	} else if md.Build != "" {
		ident.Build = md.Build
		applied = true
	}
	if md.ImageID != "" {
		ident.ImageID = md.ImageID
		applied = true
	} else if md.ExpectedImageID != "" {
		ident.ImageID = md.ExpectedImageID
		applied = true
	}
	return applied
}

func identityFromStageMeta(defaults Identity, meta any) (Identity, bool) {
	ident := defaults
	applied := false
	md, ok := jsonDecode[stageMetadata](meta)
	if ok {
		applied = applyStageMetadata(&ident, md) || applied
	}

	env, ok := jsonDecode[stageMetadataEnvelope](meta)
	if !ok {
		return ident, applied
	}
	applied = applyStageMetadata(&ident, env.Metadata) || applied
	applied = applyStageMetadata(&ident, env.Meta) || applied
	if env.Request.ExpectedImageID != "" && env.Request.Metadata.ExpectedImageID == "" {
		env.Request.Metadata.ExpectedImageID = env.Request.ExpectedImageID
	}
	if env.Request.ExpectedImageID != "" && env.Request.Meta.ExpectedImageID == "" {
		env.Request.Meta.ExpectedImageID = env.Request.ExpectedImageID
	}
	applied = applyStageMetadata(&ident, env.Request.Metadata) || applied
	applied = applyStageMetadata(&ident, env.Request.Meta) || applied
	if !applied {
		return ident, false
	}
	return ident, true
}

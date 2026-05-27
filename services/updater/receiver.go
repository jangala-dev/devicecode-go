package updater

import (
	"bytes"

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
	s.transitionTo(StateReceiving, "", "")

	if len(payload.Artefact) == 0 {
		staged, ok := consumeStreamedStage()
		if !ok {
			s.clearStagedImage()
			s.transitionTo(StateFailed, "artefact_missing", "")
			s.reply(msg, StageReply{OK: false, Err: "artefact_missing"})
			return
		}
		stageIdentity, _ := identityFromStageMeta(s.identity, payload.Meta)
		desc := StagedDescriptor{
			Version:       stageIdentity.Version,
			BuildID:       stageIdentity.Build,
			ImageID:       stageIdentity.ImageID,
			Length:        staged.Length,
			Slot:          0,
			PayloadSHA256: staged.PayloadSHA256,
		}
		if err := s.metadataWrite.WriteStagedDescriptor(desc); err != nil {
			_ = s.metadataWrite.ClearStagedDescriptor()
			s.clearStagedImage()
			s.transitionTo(StateFailed, "metadata_write_failed:"+err.Error(), "")
			s.reply(msg, StageReply{OK: false, Err: "metadata_write_failed"})
			return
		}
		s.setStagedImage(desc.ImageID, desc.Version)
		s.transitionTo(StateStaged, "", desc.Version)
		s.reply(msg, StageReply{OK: true, Stage: "staged"})
		return
	}

	sink, err := newSlotSink(uint32(len(payload.Artefact)))
	if err != nil {
		s.clearStagedImage()
		s.transitionTo(StateFailed, "sink_init_failed:"+err.Error(), "")
		s.reply(msg, StageReply{OK: false, Err: "sink_init_failed"})
		return
	}
	manifest, err := s.verifier.Verify(bytes.NewReader(payload.Artefact), sink)
	if err != nil {
		// Verifier rejected the artefact. Clear any prior descriptor so a
		// following commit cannot apply stale firmware from an older stage.
		_ = s.metadataWrite.ClearStagedDescriptor()
		s.clearStagedImage()
		s.transitionTo(StateFailed, err.Error(), "")
		s.reply(msg, StageReply{OK: false, Err: err.Error()})
		return
	}

	// On verifier success the sink holds the verified payload bytes.
	// Persist the staged descriptor via the abupdate metadata writer
	// (W11) so the next prepare/commit RPC and the next boot's
	// software fact see payload_sha256 + descriptor. The fabric-update
	// branch ships an in-memory writer; fabric-security replaces it
	// with a flash-backed implementation that survives reboots.
	if err := sink.Commit(); err != nil {
		_ = s.metadataWrite.ClearStagedDescriptor()
		s.clearStagedImage()
		s.transitionTo(StateFailed, "sink_commit_failed:"+err.Error(), "")
		s.reply(msg, StageReply{OK: false, Err: "sink_commit_failed"})
		return
	}
	desc := StagedDescriptor{
		Version:       manifest.Version,
		BuildID:       manifest.BuildID,
		ImageID:       manifest.ImageID,
		Length:        manifest.PayloadLength,
		Slot:          0, // slot-pick comes from abupdate when fabric-security wires it
		PayloadSHA256: manifest.PayloadSHA256,
	}
	if err := s.metadataWrite.WriteStagedDescriptor(desc); err != nil {
		_ = s.metadataWrite.ClearStagedDescriptor()
		s.clearStagedImage()
		s.transitionTo(StateFailed, "metadata_write_failed:"+err.Error(), "")
		s.reply(msg, StageReply{OK: false, Err: "metadata_write_failed"})
		return
	}

	s.setStagedImage(desc.ImageID, manifest.Version)
	s.transitionTo(StateStaged, "", manifest.Version)
	// Do not republish the software fact here: PayloadSHA256 describes the
	// running image, while this descriptor describes the staged image.
	s.reply(msg, StageReply{OK: true, Stage: "staged"})
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

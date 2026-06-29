package updater

// PublishSoftware emits the retained state/self/software fact with the
// build identity + the per-boot RAM-only boot_id + the persisted
// payload_sha256 (when abupdate has populated it). Callers don't pass
// inputs — the fact pulls everything from the Service's configured
// Identity + boot_id cache + metadata reader.
func (s *Service) PublishSoftware() {
	fact := SoftwareFact{
		Version:       s.identity.Version,
		BuildID:       s.identity.Build,
		ImageID:       s.identity.ImageID,
		BootID:        s.ensureBootID(),
		PayloadSHA256: s.metadata.PayloadSHA256(),
	}
	s.conn.Publish(s.conn.NewMessage(TopicSoftwareFact, fact, true))
}

func strPtrOrNil(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func int32PtrOrNil(v int32) *int32 {
	if v == 0 {
		return nil
	}
	return &v
}

// PublishUpdater emits the retained state/self/updater fact with the
// canonical {state, last_error, pending_version} shape. Called on
// every state transition (via transitionTo) and as part of the post-
// hello_ack republish.
func (s *Service) PublishUpdater() {
	s.mu.Lock()
	fact := UpdaterFact{
		State:          s.state,
		LastError:      strPtrOrNil(s.lastError),
		PendingVersion: strPtrOrNil(s.pendingVersion),
		PendingImageID: strPtrOrNil(s.pendingImageID),
		StagedImageID:  strPtrOrNil(s.stagedImageID),
		JobID:          strPtrOrNil(s.jobID),
		BootBuyRC:      int32PtrOrNil(s.bootBuyRC),
	}
	s.mu.Unlock()
	s.conn.Publish(s.conn.NewMessage(TopicUpdaterFact, fact, true))
}

// PublishHealth emits the retained state/self/health fact. Reason is
// optional; "" is dropped via the omitempty tag.
func (s *Service) PublishHealth(state, reason string) {
	fact := HealthFact{State: state, Reason: reason}
	s.conn.Publish(s.conn.NewMessage(TopicHealthFact, fact, true))
}

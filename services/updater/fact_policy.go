package updater

import "time"

func (s *Service) criticalFactLivenessInterval() time.Duration {
	if s.criticalFacts.LivenessInterval > 0 {
		return s.criticalFacts.LivenessInterval
	}
	return defaultCriticalFactLivenessInterval
}

func updaterFactsEqual(a, b UpdaterFact) bool {
	return a.State == b.State &&
		stringPtrEqual(a.LastError, b.LastError) &&
		stringPtrEqual(a.PendingVersion, b.PendingVersion) &&
		stringPtrEqual(a.PendingImageID, b.PendingImageID) &&
		stringPtrEqual(a.StagedImageID, b.StagedImageID) &&
		stringPtrEqual(a.JobID, b.JobID) &&
		int32PtrEqual(a.BootBuyRC, b.BootBuyRC)
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func int32PtrEqual(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

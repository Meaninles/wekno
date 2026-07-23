package knowledgeaux

import (
	"context"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// RunStartupBackfill is invoked by DI before schedulers, housekeeping, task
// workers, and HTTP routing are started. Individual quarantined paths do not
// fail startup; structural/time-limit errors are logged and retried later.
func RunStartupBackfill(recovery *Recovery) {
	if recovery == nil {
		return
	}
	if _, err := recovery.RunBackfill(context.Background()); err != nil {
		logger.Errorf(context.Background(), "[knowledge aux] startup storage binding backfill failed: %v", err)
	}
}

func StartRecovery(recovery *Recovery, cleaner interfaces.ResourceCleaner) {
	if recovery == nil {
		return
	}
	recovery.Start(context.Background())
	if cleaner != nil {
		cleaner.RegisterWithName("KnowledgeAuxiliaryObjectRecovery", func() error {
			recovery.Stop()
			return nil
		})
	}
}

package enrichmentrecovery

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func StartRecovery(recovery *Recovery, cleaner interfaces.ResourceCleaner) {
	if recovery == nil {
		return
	}
	recovery.Start(context.Background())
	if cleaner == nil {
		return
	}
	cleaner.RegisterWithName("EnrichmentFanoutRecovery", func() error {
		recovery.Stop()
		return nil
	})
}

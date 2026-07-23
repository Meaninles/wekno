package corefanout

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// StartRecovery is the module's only lifecycle registration point. The
// container invokes it after async/Lite handlers exist so the immediate scan
// always has a consumer.
func StartRecovery(recovery *Recovery, cleaner interfaces.ResourceCleaner) {
	if recovery == nil {
		return
	}
	recovery.Start(context.Background())
	if cleaner == nil {
		return
	}
	cleaner.RegisterWithName("CoreFanoutRecovery", func() error {
		recovery.Stop()
		return nil
	})
}

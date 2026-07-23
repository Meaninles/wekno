package wikiqueue

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// StartRecovery is the module's lifecycle registration point. Keeping start
// and shutdown wiring here lets the native container retain only a Provide
// and an Invoke call. Invoke it after the asynq server (or Lite-mode handlers)
// has been registered so the immediate recovery scan always has a consumer.
func StartRecovery(recovery *Recovery, cleaner interfaces.ResourceCleaner) {
	if recovery == nil {
		return
	}
	recovery.Start(context.Background())
	if cleaner == nil {
		return
	}
	cleaner.RegisterWithName("WikiQueueRecovery", func() error {
		recovery.Stop()
		return nil
	})
}

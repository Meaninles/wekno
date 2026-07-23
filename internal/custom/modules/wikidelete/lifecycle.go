package wikidelete

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func StartRecovery(recovery *Recovery, cleaner interfaces.ResourceCleaner) {
	if recovery == nil {
		return
	}
	recovery.Start(context.Background())
	if cleaner != nil {
		cleaner.RegisterWithName("KnowledgeDeleteRecovery", func() error {
			recovery.Stop()
			return nil
		})
	}
}

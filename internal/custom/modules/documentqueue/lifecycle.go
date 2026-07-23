package documentqueue

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func Start(coordinator *Coordinator, cleaner interfaces.ResourceCleaner) error {
	if coordinator == nil {
		return nil
	}
	if err := coordinator.Start(context.Background()); err != nil {
		return err
	}
	if cleaner != nil {
		cleaner.RegisterWithName("DocumentQueueCoordinator", func() error {
			coordinator.Stop()
			return nil
		})
	}
	return nil
}

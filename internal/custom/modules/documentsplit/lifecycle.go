package documentsplit

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func Start(manager *Manager, cleaner interfaces.ResourceCleaner) error {
	if manager == nil {
		return nil
	}
	if err := manager.Migrate(context.Background()); err != nil {
		return fmt.Errorf("document split migrate: %w", err)
	}
	manager.mu.Lock()
	if manager.cancel != nil {
		manager.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager.cancel = cancel
	manager.done = make(chan struct{})
	done := manager.done
	manager.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(manager.config.RecoveryInterval)
		defer ticker.Stop()
		if err := manager.recoverOnce(ctx); err != nil {
			logger.Warnf(ctx, "[document split] initial recovery failed: %v", err)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := manager.recoverOnce(ctx); err != nil {
					logger.Warnf(ctx, "[document split] recovery failed: %v", err)
				}
			}
		}
	}()
	if cleaner != nil {
		cleaner.RegisterWithName("DocumentSplitManager", func() error {
			manager.Stop()
			return nil
		})
	}
	return nil
}

func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.cancel = nil
	m.done = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
}

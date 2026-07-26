package service

import (
	"context"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

// SessionDeletedHook is a custom resource-cleanup extension point. Session
// deletion is committed first; hooks run asynchronously and must be
// idempotent because their durable housekeeping lanes may retry later.
type SessionDeletedHook func(ctx context.Context, tenantID uint64, userID string, sessionIDs []string) error

var sessionDeletedHooks struct {
	sync.RWMutex
	items []SessionDeletedHook
}

func RegisterSessionDeletedHook(hook SessionDeletedHook) {
	if hook == nil {
		return
	}
	sessionDeletedHooks.Lock()
	sessionDeletedHooks.items = append(sessionDeletedHooks.items, hook)
	sessionDeletedHooks.Unlock()
}

func notifySessionDeleted(ctx context.Context, tenantID uint64, userID string, sessionIDs []string) {
	if tenantID == 0 || len(sessionIDs) == 0 {
		return
	}
	sessionDeletedHooks.RLock()
	hooks := append([]SessionDeletedHook(nil), sessionDeletedHooks.items...)
	sessionDeletedHooks.RUnlock()
	if len(hooks) == 0 {
		return
	}
	ids := append([]string(nil), sessionIDs...)
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	for _, hook := range hooks {
		current := hook
		go func() {
			hookCtx, cancel := context.WithTimeout(base, 5*time.Minute)
			defer cancel()
			if err := current(hookCtx, tenantID, userID, ids); err != nil {
				logger.Warnf(hookCtx, "session deletion resource cleanup will be retried by housekeeping: %v", err)
			}
		}()
	}
}

package service

import (
	"context"
	"errors"
	"sync"
)

// KnowledgeDeleteCompletedHook lets custom resource owners finalize their own
// metadata after the native knowledge deletion pipeline has completed all
// document, object, vector and Wiki cleanup.
type KnowledgeDeleteCompletedHook func(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeIDs []string,
) error

var knowledgeDeleteCompletedHooks struct {
	sync.RWMutex
	items []KnowledgeDeleteCompletedHook
}

func RegisterKnowledgeDeleteCompletedHook(hook KnowledgeDeleteCompletedHook) {
	if hook == nil {
		return
	}
	knowledgeDeleteCompletedHooks.Lock()
	knowledgeDeleteCompletedHooks.items = append(knowledgeDeleteCompletedHooks.items, hook)
	knowledgeDeleteCompletedHooks.Unlock()
}

func notifyKnowledgeDeleteCompleted(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeIDs []string,
) error {
	if tenantID == 0 || len(knowledgeIDs) == 0 {
		return nil
	}
	knowledgeDeleteCompletedHooks.RLock()
	hooks := append([]KnowledgeDeleteCompletedHook(nil), knowledgeDeleteCompletedHooks.items...)
	knowledgeDeleteCompletedHooks.RUnlock()

	ids := append([]string(nil), knowledgeIDs...)
	var hookErrors []error
	for _, hook := range hooks {
		if err := hook(ctx, tenantID, knowledgeBaseID, ids); err != nil {
			hookErrors = append(hookErrors, err)
		}
	}
	return errors.Join(hookErrors...)
}

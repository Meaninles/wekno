package service

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
)

// KnowledgeBaseCreationDefaultsHook lets the custom control plane complete
// defaults after the native type/index defaults have been normalized and
// before the knowledge base is validated and persisted.
//
// The hook is intentionally small: model selection and product-specific
// defaults stay outside the native service, while every creation caller still
// passes through the same service boundary.
type KnowledgeBaseCreationDefaultsHook func(context.Context, *types.KnowledgeBase) error

var knowledgeBaseCreationDefaultsHooks struct {
	sync.RWMutex
	items []KnowledgeBaseCreationDefaultsHook
}

// RegisterKnowledgeBaseCreationDefaults registers a creation-defaults hook.
func RegisterKnowledgeBaseCreationDefaults(hook KnowledgeBaseCreationDefaultsHook) {
	if hook == nil {
		return
	}
	knowledgeBaseCreationDefaultsHooks.Lock()
	knowledgeBaseCreationDefaultsHooks.items = append(knowledgeBaseCreationDefaultsHooks.items, hook)
	knowledgeBaseCreationDefaultsHooks.Unlock()
}

func applyKnowledgeBaseCreationDefaults(ctx context.Context, kb *types.KnowledgeBase) error {
	if kb == nil {
		return nil
	}
	knowledgeBaseCreationDefaultsHooks.RLock()
	hooks := append([]KnowledgeBaseCreationDefaultsHook(nil), knowledgeBaseCreationDefaultsHooks.items...)
	knowledgeBaseCreationDefaultsHooks.RUnlock()
	for _, hook := range hooks {
		if err := hook(ctx, kb); err != nil {
			return err
		}
	}
	return nil
}

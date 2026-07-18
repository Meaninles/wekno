package service

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
)

// AgentRuntimeConfigHook lets custom agent types enforce request-specific
// scope after native KB resolution and before search targets/tool registration.
type AgentRuntimeConfigHook func(context.Context, *types.QARequest, *types.AgentConfig) error

var agentRuntimeConfigHooks = struct {
	sync.RWMutex
	items []AgentRuntimeConfigHook
}{}

func RegisterAgentRuntimeConfigHook(hook AgentRuntimeConfigHook) {
	if hook == nil {
		return
	}
	agentRuntimeConfigHooks.Lock()
	defer agentRuntimeConfigHooks.Unlock()
	agentRuntimeConfigHooks.items = append(agentRuntimeConfigHooks.items, hook)
}

func applyAgentRuntimeConfigHooks(ctx context.Context, req *types.QARequest, config *types.AgentConfig) error {
	agentRuntimeConfigHooks.RLock()
	hooks := append([]AgentRuntimeConfigHook(nil), agentRuntimeConfigHooks.items...)
	agentRuntimeConfigHooks.RUnlock()
	for _, hook := range hooks {
		if err := hook(ctx, req, config); err != nil {
			return err
		}
	}
	return nil
}

package session

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// AgentQARunner is a custom smart-reasoning runtime that can take over an
// AgentQA request for a specific CustomAgentConfig.AgentType.
type AgentQARunner func(ctx context.Context, req *types.QARequest, eventBus *event.EventBus) error

var agentQARunnerRegistry = struct {
	sync.RWMutex
	byType map[string]AgentQARunner
}{
	byType: map[string]AgentQARunner{},
}

// RegisterAgentQARunner registers a custom AgentQA runtime for an agent_type.
// Native AgentQA remains the fallback when no runner is registered.
func RegisterAgentQARunner(agentType string, runner AgentQARunner) {
	if agentType == "" || runner == nil {
		return
	}
	agentQARunnerRegistry.Lock()
	defer agentQARunnerRegistry.Unlock()
	agentQARunnerRegistry.byType[agentType] = runner
}

func agentQARunnerFor(agentType string) AgentQARunner {
	if agentType == "" {
		return nil
	}
	agentQARunnerRegistry.RLock()
	defer agentQARunnerRegistry.RUnlock()
	return agentQARunnerRegistry.byType[agentType]
}

// RunAgentQA dispatches an AgentQA request through the registered custom
// runtime for the agent_type when one exists, falling back to native AgentQA.
func RunAgentQA(ctx context.Context, sessionService interfaces.SessionService, req *types.QARequest, eventBus *event.EventBus) error {
	if req != nil && req.CustomAgent != nil {
		if runner := agentQARunnerFor(req.CustomAgent.Config.AgentType); runner != nil {
			return runner(ctx, req, eventBus)
		}
	}
	return sessionService.AgentQA(ctx, req, eventBus)
}

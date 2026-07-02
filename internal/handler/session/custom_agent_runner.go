package session

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
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

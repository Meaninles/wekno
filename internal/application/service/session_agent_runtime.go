package service

import (
	"context"
	"errors"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"sync"
)

type AgentRuntime func(context.Context, *types.QARequest, *event.EventBus) error

var unifiedAgent struct {
	sync.RWMutex
	run AgentRuntime
}

// RegisterAgentRuntime is the single custom registration point for every QA
// channel and profile. Missing registration is a startup/configuration error.
func RegisterAgentRuntime(run AgentRuntime) {
	if run == nil {
		panic("agent runtime must not be nil")
	}
	unifiedAgent.Lock()
	defer unifiedAgent.Unlock()
	unifiedAgent.run = run
}
func runUnifiedAgent(ctx context.Context, req *types.QARequest, bus *event.EventBus) error {
	unifiedAgent.RLock()
	run := unifiedAgent.run
	unifiedAgent.RUnlock()
	if run == nil {
		return errors.New("unified agent runtime is not registered")
	}
	return run(ctx, req, bus)
}

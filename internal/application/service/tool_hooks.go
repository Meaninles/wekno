package service

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

type RuntimeToolRegistrar func(context.Context, *tools.ToolRegistry, *types.AgentConfig, string) error
type RuntimeToolRecognizer func(context.Context, *types.AgentConfig, string) bool

var toolHookRegistry = struct {
	sync.RWMutex
	registrars  []RuntimeToolRegistrar
	recognizers []RuntimeToolRecognizer
}{}

func RegisterRuntimeToolRegistrar(fn RuntimeToolRegistrar) {
	if fn == nil {
		return
	}
	toolHookRegistry.Lock()
	defer toolHookRegistry.Unlock()
	toolHookRegistry.registrars = append(toolHookRegistry.registrars, fn)
}

func RegisterRuntimeToolRecognizer(fn RuntimeToolRecognizer) {
	if fn == nil {
		return
	}
	toolHookRegistry.Lock()
	defer toolHookRegistry.Unlock()
	toolHookRegistry.recognizers = append(toolHookRegistry.recognizers, fn)
}

func isRuntimeToolRecognized(ctx context.Context, config *types.AgentConfig, toolName string) bool {
	toolHookRegistry.RLock()
	recognizers := append([]RuntimeToolRecognizer(nil), toolHookRegistry.recognizers...)
	toolHookRegistry.RUnlock()
	for _, recognizer := range recognizers {
		if recognizer(ctx, config, toolName) {
			return true
		}
	}
	return false
}

func registerRuntimeTools(ctx context.Context, registry *tools.ToolRegistry, config *types.AgentConfig, sessionID string) {
	toolHookRegistry.RLock()
	registrars := append([]RuntimeToolRegistrar(nil), toolHookRegistry.registrars...)
	toolHookRegistry.RUnlock()
	for _, registrar := range registrars {
		if err := registrar(ctx, registry, config, sessionID); err != nil {
			logger.Warnf(ctx, "runtime tool registrar failed: %v", err)
		}
	}
}

package service

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
)

// BuiltinAgentConfigOverlay lets custom modules adjust built-in agent config
// after native defaults and tenant overrides have been resolved.
type BuiltinAgentConfigOverlay func(ctx context.Context, agent *types.CustomAgent, tenantID uint64) (*types.CustomAgent, error)

var (
	builtinAgentConfigOverlaysMu sync.RWMutex
	builtinAgentConfigOverlays   []BuiltinAgentConfigOverlay
)

func RegisterBuiltinAgentConfigOverlay(overlay BuiltinAgentConfigOverlay) {
	if overlay == nil {
		return
	}
	builtinAgentConfigOverlaysMu.Lock()
	defer builtinAgentConfigOverlaysMu.Unlock()
	builtinAgentConfigOverlays = append(builtinAgentConfigOverlays, overlay)
}

func applyBuiltinAgentConfigOverlays(
	ctx context.Context,
	agent *types.CustomAgent,
	tenantID uint64,
) (*types.CustomAgent, error) {
	if agent == nil || !types.IsBuiltinAgentID(agent.ID) {
		return agent, nil
	}

	builtinAgentConfigOverlaysMu.RLock()
	overlays := append([]BuiltinAgentConfigOverlay(nil), builtinAgentConfigOverlays...)
	builtinAgentConfigOverlaysMu.RUnlock()

	var err error
	for _, overlay := range overlays {
		agent, err = overlay(ctx, agent, tenantID)
		if err != nil {
			return nil, err
		}
		if agent == nil {
			return nil, nil
		}
	}
	return agent, nil
}

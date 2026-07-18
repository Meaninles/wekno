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
	builtinAgentConfigOverlaysMu   sync.RWMutex
	builtinAgentConfigOverlays     []BuiltinAgentConfigOverlay
	customAgentConfigNormalizersMu sync.RWMutex
	customAgentConfigNormalizers   []CustomAgentConfigNormalizer
)

// CustomAgentConfigNormalizer lets a custom module validate and normalize its
// persisted agent-type configuration without placing the implementation in the
// native custom-agent service. Returning an error rejects create/update/copy.
type CustomAgentConfigNormalizer func(ctx context.Context, agent *types.CustomAgent) error

func RegisterCustomAgentConfigNormalizer(normalizer CustomAgentConfigNormalizer) {
	if normalizer == nil {
		return
	}
	customAgentConfigNormalizersMu.Lock()
	defer customAgentConfigNormalizersMu.Unlock()
	customAgentConfigNormalizers = append(customAgentConfigNormalizers, normalizer)
}

func applyCustomAgentConfigNormalizers(ctx context.Context, agent *types.CustomAgent) error {
	customAgentConfigNormalizersMu.RLock()
	normalizers := append([]CustomAgentConfigNormalizer(nil), customAgentConfigNormalizers...)
	customAgentConfigNormalizersMu.RUnlock()
	for _, normalizer := range normalizers {
		if err := normalizer(ctx, agent); err != nil {
			return err
		}
	}
	return nil
}

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

package service

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
)

// ErrBuiltinAgentPolicyUnavailable is returned when a policy-backed endpoint
// is reached before the custom bootstrap has registered its policy service.
var ErrBuiltinAgentPolicyUnavailable = errors.New("built-in agent policy is unavailable")

// BuiltinAgentConfigOverlay lets custom modules adjust built-in agent config
// after native defaults and tenant overrides have been resolved.
type BuiltinAgentConfigOverlay func(ctx context.Context, agent *types.CustomAgent, tenantID uint64) (*types.CustomAgent, error)

// BuiltinAgentConfigMutator validates and normalizes centrally controlled
// built-in-agent fields during persistence. The requested agent is mutable so
// the native custom-agent service can persist the tenant-local effective IDs.
type BuiltinAgentConfigMutator func(
	ctx context.Context,
	previous *types.CustomAgent,
	requested *types.CustomAgent,
	tenantID uint64,
) error

// BuiltinAgentVisibilityResolver returns the tenant-wide conversation
// visibility of a built-in agent.
type BuiltinAgentVisibilityResolver func(ctx context.Context, agentID string, tenantID uint64) (bool, error)

// BuiltinAgentVisibilityUpdater persists the tenant administrator's choice of
// whether a built-in agent is available in the conversation picker.
type BuiltinAgentVisibilityUpdater func(ctx context.Context, agentID string, tenantID uint64, visible bool) error

var (
	builtinAgentConfigOverlaysMu   sync.RWMutex
	builtinAgentConfigOverlays     []BuiltinAgentConfigOverlay
	builtinAgentConfigMutatorsMu   sync.RWMutex
	builtinAgentConfigMutators     []BuiltinAgentConfigMutator
	builtinAgentVisibilityMu       sync.RWMutex
	builtinAgentVisibilityResolver BuiltinAgentVisibilityResolver
	builtinAgentVisibilityUpdater  BuiltinAgentVisibilityUpdater
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

// RegisterBuiltinAgentConfigMutator registers a centrally controlled-field
// mutator. Hooks execute in registration order.
func RegisterBuiltinAgentConfigMutator(mutator BuiltinAgentConfigMutator) {
	if mutator == nil {
		return
	}
	builtinAgentConfigMutatorsMu.Lock()
	defer builtinAgentConfigMutatorsMu.Unlock()
	builtinAgentConfigMutators = append(builtinAgentConfigMutators, mutator)
}

func applyBuiltinAgentConfigMutators(
	ctx context.Context,
	previous *types.CustomAgent,
	requested *types.CustomAgent,
	tenantID uint64,
) error {
	builtinAgentConfigMutatorsMu.RLock()
	mutators := append([]BuiltinAgentConfigMutator(nil), builtinAgentConfigMutators...)
	builtinAgentConfigMutatorsMu.RUnlock()
	for _, mutator := range mutators {
		if err := mutator(ctx, previous, requested, tenantID); err != nil {
			return err
		}
	}
	return nil
}

// RegisterBuiltinAgentVisibilityResolver registers the tenant-wide visibility
// resolver used by the native agent list/visibility endpoint.
func RegisterBuiltinAgentVisibilityResolver(resolver BuiltinAgentVisibilityResolver) {
	if resolver == nil {
		return
	}
	builtinAgentVisibilityMu.Lock()
	defer builtinAgentVisibilityMu.Unlock()
	builtinAgentVisibilityResolver = resolver
}

func RegisterBuiltinAgentVisibilityUpdater(updater BuiltinAgentVisibilityUpdater) {
	if updater == nil {
		return
	}
	builtinAgentVisibilityMu.Lock()
	defer builtinAgentVisibilityMu.Unlock()
	builtinAgentVisibilityUpdater = updater
}

// ResolveBuiltinAgentVisibility is fail-open only when the custom policy
// module is disabled. Normal production bootstrap always registers a resolver
// and therefore uses the persisted tenant policy/defaults.
func ResolveBuiltinAgentVisibility(ctx context.Context, agentID string, tenantID uint64) (bool, error) {
	builtinAgentVisibilityMu.RLock()
	resolver := builtinAgentVisibilityResolver
	builtinAgentVisibilityMu.RUnlock()
	if resolver == nil {
		return true, nil
	}
	return resolver(ctx, agentID, tenantID)
}

func UpdateBuiltinAgentVisibility(ctx context.Context, agentID string, tenantID uint64, visible bool) error {
	builtinAgentVisibilityMu.RLock()
	updater := builtinAgentVisibilityUpdater
	builtinAgentVisibilityMu.RUnlock()
	if updater == nil {
		return ErrBuiltinAgentPolicyUnavailable
	}
	return updater(ctx, agentID, tenantID, visible)
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
	if agent == nil || (!agent.IsBuiltin && !types.IsBuiltinAgentID(agent.ID) && !strings.HasPrefix(strings.TrimSpace(agent.ID), "builtin-")) {
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

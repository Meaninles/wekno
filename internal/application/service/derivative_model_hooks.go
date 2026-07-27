package service

import (
	"context"
	"errors"
	"sync"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var ErrDerivativeModelControlUnavailable = errors.New("derivative model control is unavailable")

// ModelMutation identifies the native model operation being guarded by the
// custom derivative-model control plane.
type ModelMutation string

const (
	ModelMutationCreate          ModelMutation = "create"
	ModelMutationUpdate          ModelMutation = "update"
	ModelMutationCredentials     ModelMutation = "credentials"
	ModelMutationClearCredential ModelMutation = "clear_credential"
	ModelMutationDelete          ModelMutation = "delete"
)

type DerivativeChatResolver func(
	context.Context,
	interfaces.ModelService,
	string,
) (chat.Chat, error)

type ChatModelUsageGuard func(context.Context, *types.Model) error

type ModelMutationGuard func(
	context.Context,
	ModelMutation,
	*types.Model,
	*types.Model,
) error

type KnowledgeBaseModelPolicy func(context.Context, *types.KnowledgeBase) error

var derivativeModelHooks = struct {
	sync.RWMutex
	resolver      DerivativeChatResolver
	chatGuard     ChatModelUsageGuard
	mutationGuard ModelMutationGuard
	kbPolicy      KnowledgeBaseModelPolicy
}{}

func RegisterDerivativeChatResolver(resolver DerivativeChatResolver) {
	derivativeModelHooks.Lock()
	derivativeModelHooks.resolver = resolver
	derivativeModelHooks.Unlock()
}

func RegisterChatModelUsageGuard(guard ChatModelUsageGuard) {
	derivativeModelHooks.Lock()
	derivativeModelHooks.chatGuard = guard
	derivativeModelHooks.Unlock()
}

func RegisterModelMutationGuard(guard ModelMutationGuard) {
	derivativeModelHooks.Lock()
	derivativeModelHooks.mutationGuard = guard
	derivativeModelHooks.Unlock()
}

func RegisterKnowledgeBaseModelPolicy(policy KnowledgeBaseModelPolicy) {
	derivativeModelHooks.Lock()
	derivativeModelHooks.kbPolicy = policy
	derivativeModelHooks.Unlock()
}

// GetDerivativeChatModel is the only sanctioned native entry point for
// summary, question, graph and Wiki generation. requestedModelID may be empty,
// in which case the platform default is used. It never falls back to a KB's
// interactive SummaryModelID.
func GetDerivativeChatModel(
	ctx context.Context,
	modelService interfaces.ModelService,
	requestedModelID string,
) (chat.Chat, error) {
	derivativeModelHooks.RLock()
	resolver := derivativeModelHooks.resolver
	derivativeModelHooks.RUnlock()
	if resolver == nil {
		return nil, ErrDerivativeModelControlUnavailable
	}
	return resolver(ctx, modelService, requestedModelID)
}

func guardChatModelUsage(ctx context.Context, model *types.Model) error {
	derivativeModelHooks.RLock()
	guard := derivativeModelHooks.chatGuard
	derivativeModelHooks.RUnlock()
	if guard == nil {
		if model != nil && model.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly {
			return ErrDerivativeModelControlUnavailable
		}
		return nil
	}
	return guard(ctx, model)
}

func guardModelMutation(
	ctx context.Context,
	operation ModelMutation,
	existing *types.Model,
	proposed *types.Model,
) error {
	derivativeModelHooks.RLock()
	guard := derivativeModelHooks.mutationGuard
	derivativeModelHooks.RUnlock()
	if guard == nil {
		if existing != nil &&
			existing.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly {
			return ErrDerivativeModelControlUnavailable
		}
		return nil
	}
	return guard(ctx, operation, existing, proposed)
}

func ValidateKnowledgeBaseModelPolicy(
	ctx context.Context,
	kb *types.KnowledgeBase,
) error {
	derivativeModelHooks.RLock()
	policy := derivativeModelHooks.kbPolicy
	derivativeModelHooks.RUnlock()
	if policy == nil {
		if kb != nil && kb.DerivativeModelID != "" {
			return ErrDerivativeModelControlUnavailable
		}
		return nil
	}
	return policy(ctx, kb)
}

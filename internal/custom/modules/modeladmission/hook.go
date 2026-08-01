package modeladmission

import (
	"context"
	"sync/atomic"
)

type admissionGrantedHookKey struct{}
type providerExecutionStateKey struct{}

type providerExecutionState struct {
	started atomic.Bool
	parent  *providerExecutionState
}

// WithProviderExecutionTracking creates a task-local marker shared by every
// context derived from ctx. It lets business tracing distinguish a capacity
// yield before admission from an execution that really reached a provider.
func WithProviderExecutionTracking(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	parent, _ := ctx.Value(providerExecutionStateKey{}).(*providerExecutionState)
	return context.WithValue(ctx, providerExecutionStateKey{}, &providerExecutionState{parent: parent})
}

// ProviderExecutionStarted reports whether at least one admitted provider
// call started in this tracked task context.
func ProviderExecutionStarted(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	state, _ := ctx.Value(providerExecutionStateKey{}).(*providerExecutionState)
	return state != nil && state.started.Load()
}

func ensureProviderExecutionTracking(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(providerExecutionStateKey{}).(*providerExecutionState); ok {
		return ctx
	}
	return WithProviderExecutionTracking(ctx)
}

// WithAdmissionGrantedHook registers work that must run after distributed
// admission succeeds but before the provider request starts. Durable workers
// use this boundary to consume a provider-attempt only for a real call.
func WithAdmissionGrantedHook(
	ctx context.Context,
	hook func(context.Context) error,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = ensureProviderExecutionTracking(ctx)
	if hook == nil {
		return ctx
	}
	return context.WithValue(ctx, admissionGrantedHookKey{}, hook)
}

func runAdmissionGrantedHook(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	hook, _ := ctx.Value(admissionGrantedHookKey{}).(func(context.Context) error)
	if hook != nil {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	if state, _ := ctx.Value(providerExecutionStateKey{}).(*providerExecutionState); state != nil {
		for current := state; current != nil; current = current.parent {
			current.started.Store(true)
		}
	}
	return nil
}

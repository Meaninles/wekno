package modeladmission

import (
	"context"
	"sync"
	"sync/atomic"
)

type admissionGrantedHookKey struct{}
type providerExecutionStateKey struct{}

type providerExecutionState struct {
	started   atomic.Bool
	parent    *providerExecutionState
	mu        sync.Mutex
	observers []*providerStartObserver
}

type providerStartObserver struct {
	fn      func(context.Context)
	started bool
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

// RegisterProviderStartObserver attaches best-effort observability work to the
// exact boundary after admission succeeds and before the provider is called.
// Unlike the durable admission-granted hook, observer failures cannot block or
// alter the model request. The returned bool is false when the context is not
// execution-tracked; callers can still persist a terminal result later.
func RegisterProviderStartObserver(ctx context.Context, observer func(context.Context)) bool {
	if ctx == nil || observer == nil {
		return false
	}
	state, _ := ctx.Value(providerExecutionStateKey{}).(*providerExecutionState)
	if state == nil {
		return false
	}
	state.mu.Lock()
	state.observers = append(state.observers, &providerStartObserver{fn: observer})
	state.mu.Unlock()
	return true
}

func runProviderStartObservers(ctx context.Context) {
	state, _ := ctx.Value(providerExecutionStateKey{}).(*providerExecutionState)
	if state == nil {
		return
	}
	chain := make([]*providerExecutionState, 0, 4)
	for current := state; current != nil; current = current.parent {
		chain = append(chain, current)
	}
	// Parents are aggregate business spans. Start them before their child so
	// parent lookup succeeds even when both were prepared lazily.
	for i := len(chain) - 1; i >= 0; i-- {
		current := chain[i]
		current.mu.Lock()
		pending := make([]func(context.Context), 0, len(current.observers))
		for _, observer := range current.observers {
			if observer == nil || observer.started || observer.fn == nil {
				continue
			}
			observer.started = true
			pending = append(pending, observer.fn)
		}
		current.mu.Unlock()
		for _, observer := range pending {
			observer(ctx)
		}
	}
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
	runProviderStartObservers(ctx)
	if state, _ := ctx.Value(providerExecutionStateKey{}).(*providerExecutionState); state != nil {
		for current := state; current != nil; current = current.parent {
			current.started.Store(true)
		}
	}
	return nil
}

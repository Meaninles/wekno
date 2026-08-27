package langfuse

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"go.opentelemetry.io/otel/trace"
)

var traceCtxKey = types.LangfuseTraceContextKey

func withTrace(ctx context.Context, current *Trace) context.Context {
	if ctx == nil || current == nil {
		return ctx
	}
	return context.WithValue(ctx, traceCtxKey, current)
}

func traceFromCtx(ctx context.Context) (*Trace, bool) {
	if ctx == nil {
		return nil, false
	}
	current, ok := ctx.Value(traceCtxKey).(*Trace)
	return current, ok && current != nil
}

func TraceFromContext(ctx context.Context) (*Trace, bool) {
	return traceFromCtx(ctx)
}

func withParentObservation(ctx context.Context, _ string) context.Context {
	return ctx
}

func parentObservationFromCtx(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return "", false
	}
	return spanContext.SpanID().String(), true
}

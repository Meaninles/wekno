package langfuse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/agenteval"
	"go.opentelemetry.io/otel/attribute"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func testConfig(capture bool) Config {
	policy := agenteval.CaptureMetadata
	mode := agenteval.ModeProduction
	if capture {
		policy = agenteval.CaptureFull
		mode = agenteval.ModeEval
	}
	return Config{
		Enabled: true, Host: "http://localhost", PublicKey: "pk", SecretKey: "sk",
		SampleRate: 1, QueueSize: 128, FlushAt: 16, FlushInterval: time.Millisecond,
		RequestTimeout: time.Second, MaxAttributeBytes: 4096,
		AgentEval: agenteval.Config{Mode: mode, CapturePolicy: policy},
	}
}

func attributeMap(attrs []attribute.KeyValue) map[string]string {
	result := map[string]string{}
	for _, item := range attrs {
		result[string(item.Key)] = item.Value.Emit()
	}
	return result
}

func TestOTelTraceNestingAndLangfuseAttributes(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	manager := newManagerWithExporter(testConfig(true), exporter)
	ctx, root := manager.StartTrace(context.Background(), TraceOptions{
		Name: "root", UserID: "user", SessionID: "session", Input: map[string]string{"q": "hello"},
		Tags: []string{"eval"},
	})
	outerCtx, outer := manager.StartSpan(ctx, SpanOptions{Name: "outer"})
	innerCtx, inner := manager.StartSpan(outerCtx, SpanOptions{Name: "inner"})
	_, generation := manager.StartGeneration(innerCtx, GenerationOptions{
		Name: "generation", Model: "model", Input: "prompt", ModelParameters: map[string]interface{}{"temperature": 0},
	})
	generation.MarkCompletionStart(time.Now())
	generation.Finish("answer", &TokenUsage{Input: 1, Output: 2, Total: 3, Unit: "TOKENS"}, nil)
	inner.Finish("inner-output", nil, nil)
	outer.Finish("outer-output", nil, nil)
	root.Finish("root-output", nil)
	if err := manager.provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("got %d spans", len(spans))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, span := range spans {
		byName[span.Name] = span
		if span.SpanContext.TraceID() != root.spanContext.TraceID() {
			t.Fatalf("span %s has another trace id", span.Name)
		}
	}
	if byName["outer"].Parent.SpanID() != byName["root"].SpanContext.SpanID() ||
		byName["inner"].Parent.SpanID() != byName["outer"].SpanContext.SpanID() ||
		byName["generation"].Parent.SpanID() != byName["inner"].SpanContext.SpanID() {
		t.Fatal("unexpected parent chain")
	}
	attrs := attributeMap(byName["generation"].Attributes)
	if attrs["langfuse.observation.type"] != "generation" || attrs["langfuse.user.id"] != "user" {
		t.Fatalf("missing Langfuse attributes: %#v", attrs)
	}
	if !strings.Contains(attrs["langfuse.observation.output"], "answer") ||
		!strings.Contains(attrs["langfuse.observation.usage_details"], "total") {
		t.Fatalf("missing generation output/usage: %#v", attrs)
	}
}

func TestProductionMetadataCaptureDoesNotExportRawContent(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	manager := newManagerWithExporter(testConfig(false), exporter)
	ctx, root := manager.StartTrace(context.Background(), TraceOptions{
		Name: "root", Metadata: map[string]interface{}{
			"payload": "super-secret-metadata",
			"count":   3,
		},
	})
	_, generation := manager.StartGeneration(ctx, GenerationOptions{
		Name: "generation", Input: "super-secret-prompt",
		ModelParameters: map[string]interface{}{"stop": "super-secret-stop", "temperature": 0.2},
	})
	generation.Finish("super-secret-answer", nil, nil)
	root.Finish(nil, nil)
	if err := manager.provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, span := range exporter.GetSpans() {
		for key, value := range attributeMap(span.Attributes) {
			if strings.Contains(value, "super-secret") {
				t.Fatalf("raw content leaked in %s=%s", key, value)
			}
		}
	}
}

func TestUsageDetailsContainOnlyNumericBuckets(t *testing.T) {
	tokens := usageDetails(&TokenUsage{Input: 3, Output: 4, Total: 7, Unit: "TOKENS"})
	if tokens["input"] != 3 || tokens["output"] != 4 || tokens["total"] != 7 {
		t.Fatalf("unexpected token usage: %#v", tokens)
	}
	if _, ok := tokens["unit"]; ok {
		t.Fatalf("unit must not be sent as a usage bucket: %#v", tokens)
	}
	seconds := usageDetails(&TokenUsage{Output: 12, Total: 12, Unit: "SECONDS"})
	if seconds["audio_seconds"] != 12 || seconds["total"] != 12 {
		t.Fatalf("unexpected audio usage: %#v", seconds)
	}
}

package langfuse

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type tracePayload struct {
	types.TracingContext
	Value string `json:"value"`
}

func TestInjectAndResumeAcrossAsynqUsesW3CIDs(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	manager := newManagerWithExporter(testConfig(true), exporter)
	installGlobal(manager)
	ctx, root := manager.StartTrace(context.Background(), TraceOptions{Name: "root"})
	payload := tracePayload{Value: "work"}
	InjectTracing(ctx, &payload)
	if len(payload.LangfuseTraceID) != 32 || len(payload.LangfuseParentObservationID) != 16 {
		t.Fatalf("unexpected W3C ids: %#v", payload.TracingContext)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	task := asynq.NewTask("test:task", raw)
	var workerTraceID string
	handler := AsynqMiddleware()(asynq.HandlerFunc(func(workerCtx context.Context, _ *asynq.Task) error {
		if current, ok := TraceFromContext(workerCtx); ok {
			workerTraceID = current.ID
		}
		return nil
	}))
	if err := handler.ProcessTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	root.Finish(nil, nil)
	if workerTraceID != payload.LangfuseTraceID {
		t.Fatalf("worker trace=%s payload trace=%s", workerTraceID, payload.LangfuseTraceID)
	}
}

func TestSpanInputFromPayloadIsBounded(t *testing.T) {
	value := spanInputFromPayload(make([]byte, 2048), true).(map[string]interface{})
	if value["bytes"] != 2048 {
		t.Fatalf("unexpected summary: %#v", value)
	}
}

func TestProductionSpanInputDoesNotContainPayload(t *testing.T) {
	value := spanInputFromPayload([]byte(`{"secret":"do-not-record"}`), false).(map[string]interface{})
	if len(value) != 1 || value["bytes"] != 26 {
		t.Fatalf("unexpected metadata-only value: %#v", value)
	}
}

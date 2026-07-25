package pipelineobs

import (
	"context"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	ExecutorInstanceIDMetadataKey = "executor_instance_id"
	ExecutorBootIDMetadataKey     = "executor_boot_id"
	ExecutorTaskTypeMetadataKey   = "executor_task_type"
)

type executionContextKey struct{}

// Execution identifies the exact application replica and boot that invoked an
// Asynq handler. It deliberately contains no tenant, document or user labels.
// The value is copied into the existing document-processing span metadata so
// operators can prove stage placement and failover without scraping logs.
type Execution struct {
	InstanceID string
	BootID     string
	TaskType   string
}

var (
	taskExecutions = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "weknora",
			Subsystem: "document_stage",
			Name:      "executions_total",
			Help:      "Document pipeline task executions in this application replica.",
		},
		[]string{"stage", "outcome"},
	)
	taskExecutionDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "weknora",
			Subsystem: "document_stage",
			Name:      "execution_seconds",
			Help:      "Document pipeline task handler duration in this application replica.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 180, 600, 1800},
		},
		[]string{"stage", "outcome"},
	)
)

// WithExecution annotates a handler context with a bounded, non-secret worker
// identity. Blank identities are not useful evidence and are therefore not
// installed.
func WithExecution(ctx context.Context, instanceID, bootID, taskType string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	execution := Execution{
		InstanceID: strings.TrimSpace(instanceID),
		BootID:     strings.TrimSpace(bootID),
		TaskType:   strings.TrimSpace(taskType),
	}
	if execution.InstanceID == "" || execution.BootID == "" {
		return ctx
	}
	return context.WithValue(ctx, executionContextKey{}, execution)
}

func ExecutionFromContext(ctx context.Context) (Execution, bool) {
	if ctx == nil {
		return Execution{}, false
	}
	execution, ok := ctx.Value(executionContextKey{}).(Execution)
	if !ok || execution.InstanceID == "" || execution.BootID == "" {
		return Execution{}, false
	}
	return execution, true
}

// DocumentTaskStage maps the closed set of document task types to bounded
// metric labels. Unknown jobs are collapsed into "other" so a task name can
// never create unbounded Prometheus cardinality.
func DocumentTaskStage(taskType string) string {
	switch strings.TrimSpace(taskType) {
	case "document:process", "manual:process":
		return "document"
	case "document:split_part":
		return "split_part"
	case "document:split_finalize":
		return "split_finalize"
	case "knowledge:post_process":
		return "postprocess"
	case "summary:generation":
		return "summary"
	case "question:generation":
		return "questions"
	case "chunk:extract":
		return "graph"
	case "wiki:ingest":
		return "wiki"
	case "image:multimodal":
		return "multimodal"
	case "datatable:summary":
		return "table"
	case "knowledge:terminal_repair":
		return "terminal_repair"
	default:
		return "other"
	}
}

// AsynqExecutionMiddleware supplies the exact replica identity to every
// document span created by a handler and records process-local counters. Since
// Prometheus scrapes each Pod separately, the metric intentionally does not
// repeat instance_id as a label.
func AsynqExecutionMiddleware(instanceID, bootID string) asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			taskType := ""
			if task != nil {
				taskType = task.Type()
			}
			stage := DocumentTaskStage(taskType)
			started := time.Now()
			err := next.ProcessTask(WithExecution(ctx, instanceID, bootID, taskType), task)
			outcome := "success"
			if err != nil {
				outcome = "error"
			}
			taskExecutions.WithLabelValues(stage, outcome).Inc()
			taskExecutionDuration.WithLabelValues(stage, outcome).Observe(time.Since(started).Seconds())
			return err
		})
	}
}

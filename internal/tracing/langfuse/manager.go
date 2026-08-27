package langfuse

import (
	"context"
	"encoding/base64"
	"sync"
	"sync/atomic"

	"github.com/Tencent/WeKnora/internal/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type Manager struct {
	cfg      Config
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	closed   atomic.Bool
}

var (
	globalMu sync.RWMutex
	global   *Manager
)

func Init(cfg Config) (*Manager, error) {
	manager := &Manager{cfg: cfg}
	if cfg.AgentEval.Warning != "" {
		logger.Warnf(context.Background(), "[Langfuse] %s", cfg.AgentEval.Warning)
	}
	if err := cfg.Validate(); err != nil {
		logger.Warnf(context.Background(), "[Langfuse] disabled after invalid config: %v", err)
		manager.cfg.Enabled = false
		installGlobal(manager)
		return manager, nil
	}
	if !cfg.Enabled || cfg.SampleRate == 0 {
		installGlobal(manager)
		return manager, nil
	}
	authorization := "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.PublicKey+":"+cfg.SecretKey))
	exporter, err := otlptracehttp.New(
		context.Background(),
		otlptracehttp.WithEndpointURL(cfg.OTLPTraceEndpoint()),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization":                authorization,
			"x-langfuse-ingestion-version": "4",
		}),
		otlptracehttp.WithTimeout(cfg.RequestTimeout),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
	)
	if err != nil {
		logger.Warnf(context.Background(), "[Langfuse] OTLP exporter init failed; recorder disabled: %v", err)
		manager.cfg.Enabled = false
		installGlobal(manager)
		return manager, nil
	}
	manager = newManagerWithExporter(cfg, exporter)
	installGlobal(manager)
	logger.Infof(
		context.Background(),
		"[Langfuse] OTLP enabled host=%s mode=%s capture=%s sample_rate=%.3f queue=%d",
		cfg.Host, cfg.AgentEval.Mode, cfg.AgentEval.CapturePolicy, cfg.SampleRate, cfg.QueueSize,
	)
	return manager, nil
}

func newManagerWithExporter(cfg Config, exporter sdktrace.SpanExporter) *Manager {
	res := resource.NewSchemaless(
		attribute.String("service.name", "weknora"),
		attribute.String("service.version", cfg.Release),
		attribute.String("deployment.environment.name", cfg.Environment),
	)
	processor := sdktrace.NewBatchSpanProcessor(
		exporter,
		sdktrace.WithMaxQueueSize(cfg.QueueSize),
		sdktrace.WithMaxExportBatchSize(cfg.FlushAt),
		sdktrace.WithBatchTimeout(cfg.FlushInterval),
		sdktrace.WithExportTimeout(cfg.RequestTimeout),
	)
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(processor),
	)
	return &Manager{
		cfg:      cfg,
		provider: provider,
		tracer:   provider.Tracer("github.com/Tencent/WeKnora/internal/tracing/langfuse"),
	}
}

func installGlobal(manager *Manager) {
	globalMu.Lock()
	global = manager
	globalMu.Unlock()
}

func GetManager() *Manager {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global
}

func (m *Manager) Enabled() bool {
	return m != nil && m.cfg.Enabled && m.cfg.SampleRate > 0 && m.provider != nil && !m.closed.Load()
}

// EnabledFor is the hot-path guard used by model adapters. An unsampled
// parent is a final decision: adapters must call the business implementation
// directly instead of rebuilding summaries or wrapping streaming channels.
func (m *Manager) EnabledFor(ctx context.Context) bool {
	if !m.Enabled() {
		return false
	}
	if current, ok := traceFromCtx(ctx); ok && current != nil {
		return current.Recording()
	}
	return true
}

func (m *Manager) CaptureContent() bool {
	return m != nil && m.Enabled() && m.cfg.CaptureContent()
}

func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil || m.provider == nil || !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	return m.provider.Shutdown(ctx)
}

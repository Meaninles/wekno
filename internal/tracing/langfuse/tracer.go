package langfuse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type TokenUsage struct {
	Input  int    `json:"input,omitempty"`
	Output int    `json:"output,omitempty"`
	Total  int    `json:"total,omitempty"`
	Unit   string `json:"unit,omitempty"`
}

type traceProperties struct {
	Name        string
	UserID      string
	SessionID   string
	Metadata    map[string]interface{}
	Tags        []string
	Environment string
	Release     string
}

type Trace struct {
	ID          string
	manager     *Manager
	sampled     bool
	span        oteltrace.Span
	spanContext oteltrace.SpanContext
	remote      bool
	properties  traceProperties
}

type Generation struct {
	ID                  string
	TraceID             string
	ParentObservationID string
	manager             *Manager
	sampled             bool
	span                oteltrace.Span
	ownedTrace          *Trace
	startTime           time.Time
	model               string
	name                string
}

type Span struct {
	ID                  string
	TraceID             string
	ParentObservationID string
	manager             *Manager
	sampled             bool
	span                oteltrace.Span
	ownedTrace          *Trace
	startTime           time.Time
	name                string
}

type TraceOptions struct {
	Name        string
	UserID      string
	SessionID   string
	Input       interface{}
	Metadata    map[string]interface{}
	Tags        []string
	Environment string
	Release     string
}

type GenerationOptions struct {
	Name            string
	Model           string
	Input           interface{}
	Metadata        map[string]interface{}
	ModelParameters map[string]interface{}
}

type SpanOptions struct {
	Name     string
	Input    interface{}
	Metadata map[string]interface{}
}

// Recording lets hot-path adapters skip all capture and buffering work when
// the OpenTelemetry sampler dropped this operation. The sampler decision is
// made before the business call and never changes its result.
func (current *Trace) Recording() bool {
	return current != nil && current.sampled && current.spanContext.IsSampled()
}

func (current *Generation) Recording() bool {
	return current != nil && current.sampled
}

func (current *Span) Recording() bool {
	return current != nil && current.sampled
}

func (m *Manager) StartTrace(ctx context.Context, opts TraceOptions) (context.Context, *Trace) {
	if !m.Enabled() {
		return ctx, &Trace{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	environment := firstNonEmpty(opts.Environment, m.cfg.Environment)
	release := firstNonEmpty(opts.Release, m.cfg.Release)
	properties := traceProperties{
		Name:        opts.Name,
		UserID:      opts.UserID,
		SessionID:   opts.SessionID,
		Metadata:    opts.Metadata,
		Tags:        append([]string(nil), opts.Tags...),
		Environment: environment,
		Release:     release,
	}
	attrs := m.sharedAttributes(properties)
	attrs = append(attrs, attribute.String("langfuse.observation.type", "span"))
	attrs = append(attrs, m.valueAttribute("langfuse.observation.input", opts.Input))
	ctx, otelSpan := m.tracer.Start(
		ctx,
		firstNonEmpty(opts.Name, "weknora.trace"),
		oteltrace.WithNewRoot(),
		oteltrace.WithAttributes(attrs...),
	)
	spanContext := otelSpan.SpanContext()
	current := &Trace{
		ID:          spanContext.TraceID().String(),
		manager:     m,
		sampled:     otelSpan.IsRecording(),
		span:        otelSpan,
		spanContext: spanContext,
		properties:  properties,
	}
	return withTrace(ctx, current), current
}

func (current *Trace) Finish(output interface{}, metadata map[string]interface{}) {
	if current == nil || current.span == nil {
		return
	}
	if current.sampled {
		attrs := []attribute.KeyValue{current.manager.valueAttribute("langfuse.observation.output", output)}
		attrs = append(attrs, current.manager.metadataAttributes("langfuse.observation.metadata.", metadata)...)
		current.span.SetAttributes(attrs...)
	}
	current.span.End()
}

func (m *Manager) ResumeTrace(ctx context.Context, traceID, parentObservationID string) (context.Context, *Trace) {
	if !m.Enabled() || traceID == "" {
		return ctx, nil
	}
	parsedTraceID, err := oteltrace.TraceIDFromHex(strings.ReplaceAll(traceID, "-", ""))
	if err != nil || !parsedTraceID.IsValid() {
		return ctx, nil
	}
	parsedSpanID, err := oteltrace.SpanIDFromHex(parentObservationID)
	if err != nil || !parsedSpanID.IsValid() {
		return ctx, nil
	}
	spanContext := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    parsedTraceID,
		SpanID:     parsedSpanID,
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	})
	ctx = oteltrace.ContextWithRemoteSpanContext(ctx, spanContext)
	current := &Trace{
		ID:          parsedTraceID.String(),
		manager:     m,
		sampled:     true,
		spanContext: spanContext,
		remote:      true,
	}
	return withTrace(ctx, current), current
}

func (m *Manager) StartSpan(ctx context.Context, opts SpanOptions) (context.Context, *Span) {
	if !m.EnabledFor(ctx) {
		return ctx, &Span{}
	}
	current, ok := traceFromCtx(ctx)
	var ownedTrace *Trace
	if !ok || current == nil {
		ctx, current = m.StartTrace(ctx, TraceOptions{Name: opts.Name})
		ownedTrace = current
	}
	ctx = parentContext(ctx, current)
	parent := oteltrace.SpanContextFromContext(ctx)
	attrs := m.sharedAttributes(current.properties)
	attrs = append(attrs, attribute.String("langfuse.observation.type", "span"))
	attrs = append(attrs, m.valueAttribute("langfuse.observation.input", opts.Input))
	attrs = append(attrs, m.metadataAttributes("langfuse.observation.metadata.", opts.Metadata)...)
	now := time.Now()
	childCtx, otelSpan := m.tracer.Start(
		ctx,
		firstNonEmpty(opts.Name, "weknora.span"),
		oteltrace.WithAttributes(attrs...),
	)
	spanContext := otelSpan.SpanContext()
	result := &Span{
		ID:                  spanContext.SpanID().String(),
		TraceID:             spanContext.TraceID().String(),
		ParentObservationID: parent.SpanID().String(),
		manager:             m,
		sampled:             otelSpan.IsRecording(),
		span:                otelSpan,
		ownedTrace:          ownedTrace,
		startTime:           now,
		name:                opts.Name,
	}
	return withTrace(childCtx, current), result
}

func (current *Span) Finish(output interface{}, metadata map[string]interface{}, err error) {
	if current == nil || current.span == nil {
		return
	}
	if current.sampled {
		attrs := []attribute.KeyValue{current.manager.valueAttribute("langfuse.observation.output", output)}
		attrs = append(attrs, current.manager.metadataAttributes("langfuse.observation.metadata.", metadata)...)
		setSpanError(current.span, &attrs, err)
		current.span.SetAttributes(attrs...)
	}
	current.span.End()
	if current.ownedTrace != nil {
		current.ownedTrace.Finish(map[string]interface{}{"auto_trace": true}, nil)
	}
}

func (m *Manager) StartGeneration(ctx context.Context, opts GenerationOptions) (context.Context, *Generation) {
	if !m.EnabledFor(ctx) {
		return ctx, &Generation{}
	}
	current, ok := traceFromCtx(ctx)
	var ownedTrace *Trace
	if !ok || current == nil {
		ctx, current = m.StartTrace(ctx, TraceOptions{Name: opts.Name})
		ownedTrace = current
	}
	ctx = parentContext(ctx, current)
	parent := oteltrace.SpanContextFromContext(ctx)
	attrs := m.sharedAttributes(current.properties)
	attrs = append(attrs,
		attribute.String("langfuse.observation.type", "generation"),
		attribute.String("langfuse.observation.model.name", opts.Model),
		m.valueAttribute("langfuse.observation.input", opts.Input),
		m.valueAttribute("langfuse.observation.model.parameters", opts.ModelParameters),
	)
	attrs = append(attrs, m.metadataAttributes("langfuse.observation.metadata.", opts.Metadata)...)
	now := time.Now()
	childCtx, otelSpan := m.tracer.Start(
		ctx,
		firstNonEmpty(opts.Name, "weknora.generation"),
		oteltrace.WithAttributes(attrs...),
	)
	spanContext := otelSpan.SpanContext()
	result := &Generation{
		ID:                  spanContext.SpanID().String(),
		TraceID:             spanContext.TraceID().String(),
		ParentObservationID: parent.SpanID().String(),
		manager:             m,
		sampled:             otelSpan.IsRecording(),
		span:                otelSpan,
		ownedTrace:          ownedTrace,
		startTime:           now,
		model:               opts.Model,
		name:                opts.Name,
	}
	return withTrace(childCtx, current), result
}

func (current *Generation) Finish(output interface{}, usage *TokenUsage, err error) {
	if current == nil || current.span == nil {
		return
	}
	if current.sampled {
		attrs := []attribute.KeyValue{current.manager.valueAttribute("langfuse.observation.output", output)}
		if usage != nil {
			attrs = append(attrs, current.manager.valueAttribute("langfuse.observation.usage_details", usageDetails(usage)))
		}
		setSpanError(current.span, &attrs, err)
		current.span.SetAttributes(attrs...)
	}
	current.span.End()
	if current.ownedTrace != nil {
		current.ownedTrace.Finish(map[string]interface{}{"auto_trace": true}, nil)
	}
}

func usageDetails(usage *TokenUsage) map[string]interface{} {
	if usage == nil {
		return nil
	}
	if strings.EqualFold(usage.Unit, "SECONDS") {
		return map[string]interface{}{
			"audio_seconds": usage.Total,
			"total":         usage.Total,
		}
	}
	return map[string]interface{}{
		"input":  usage.Input,
		"output": usage.Output,
		"total":  usage.Total,
	}
}

func (current *Generation) MarkCompletionStart(timestamp time.Time) {
	if current == nil || current.span == nil || !current.sampled {
		return
	}
	current.span.SetAttributes(attribute.String(
		"langfuse.observation.completion_start_time", timestamp.UTC().Format(time.RFC3339Nano),
	))
}

func parentContext(ctx context.Context, current *Trace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if oteltrace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	if current == nil {
		return ctx
	}
	if current.span != nil {
		return oteltrace.ContextWithSpan(ctx, current.span)
	}
	if current.spanContext.IsValid() {
		if current.remote {
			return oteltrace.ContextWithRemoteSpanContext(ctx, current.spanContext)
		}
		return oteltrace.ContextWithSpanContext(ctx, current.spanContext)
	}
	return ctx
}

func setSpanError(span oteltrace.Span, attrs *[]attribute.KeyValue, err error) {
	if err == nil {
		*attrs = append(*attrs, attribute.String("langfuse.observation.level", "DEFAULT"))
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	*attrs = append(*attrs,
		attribute.String("langfuse.observation.level", "ERROR"),
		attribute.String("langfuse.observation.status_message", err.Error()),
	)
}

func (m *Manager) sharedAttributes(properties traceProperties) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("langfuse.trace.name", properties.Name),
		attribute.String("langfuse.version", "weknora-agent-eval-v1"),
		attribute.String("langfuse.environment", properties.Environment),
		attribute.String("langfuse.release", properties.Release),
	}
	if properties.UserID != "" {
		attrs = append(attrs, attribute.String("langfuse.user.id", properties.UserID))
	}
	if properties.SessionID != "" {
		attrs = append(attrs, attribute.String("langfuse.session.id", properties.SessionID))
	}
	if len(properties.Tags) > 0 {
		attrs = append(attrs, attribute.StringSlice("langfuse.trace.tags", properties.Tags))
	}
	attrs = append(attrs, m.metadataAttributes("langfuse.trace.metadata.", properties.Metadata)...)
	return attrs
}

func (m *Manager) metadataAttributes(prefix string, metadata map[string]interface{}) []attribute.KeyValue {
	if len(metadata) == 0 {
		return nil
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	attrs := make([]attribute.KeyValue, 0, len(keys))
	for _, key := range keys {
		value := metadata[key]
		attributeKey := prefix + sanitizeAttributeKey(key)
		if !m.cfg.CaptureContent() {
			attrs = append(attrs, productionMetadataAttribute(attributeKey, value))
			continue
		}
		attrs = append(attrs, m.valueAttribute(attributeKey, value))
	}
	return attrs
}

// productionMetadataAttribute retains only numeric/boolean measurements.
// Every string, collection or object is reduced to type/size metadata even if
// a caller used an unexpected key such as "payload" instead of "content".
func productionMetadataAttribute(key string, value interface{}) attribute.KeyValue {
	if value == nil {
		return attribute.String(key, "null")
	}
	reflected := reflect.ValueOf(value)
	for reflected.IsValid() && (reflected.Kind() == reflect.Interface || reflected.Kind() == reflect.Pointer) {
		if reflected.IsNil() {
			return attribute.String(key, "null")
		}
		reflected = reflected.Elem()
	}
	if reflected.IsValid() {
		switch reflected.Kind() {
		case reflect.Bool:
			return attribute.Bool(key, reflected.Bool())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return attribute.Int64(key, reflected.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			value := reflected.Uint()
			if value <= uint64(^uint64(0)>>1) {
				return attribute.Int64(key, int64(value))
			}
		case reflect.Float32, reflect.Float64:
			return attribute.Float64(key, reflected.Float())
		}
	}
	return attribute.String(key, summarizeValue(value))
}

func sanitizeAttributeKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.NewReplacer(" ", "_", "/", "_", "\\", "_", ":", "_").Replace(key)
	if key == "" {
		return "value"
	}
	return key
}

func (m *Manager) valueAttribute(key string, value interface{}) attribute.KeyValue {
	if value == nil {
		return attribute.String(key, "null")
	}
	if !m.cfg.CaptureContent() && (key == "langfuse.observation.input" || key == "langfuse.observation.output") {
		return attribute.String(key, summarizeValue(value))
	}
	if !m.cfg.CaptureContent() && key == "langfuse.observation.model.parameters" {
		return productionMetadataAttribute(key, value)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return attribute.String(key, summarizeValue(value))
	}
	if len(encoded) <= m.cfg.MaxAttributeBytes {
		return attribute.String(key, string(encoded))
	}
	digest := sha256.Sum256(encoded)
	previewLimit := m.cfg.MaxAttributeBytes / 4
	if previewLimit < 256 {
		previewLimit = 256
	}
	if previewLimit > len(encoded) {
		previewLimit = len(encoded)
	}
	bounded, _ := json.Marshal(map[string]interface{}{
		"truncated": true,
		"bytes":     len(encoded),
		"sha256":    hex.EncodeToString(digest[:]),
		"preview":   string(encoded[:previewLimit]),
	})
	return attribute.String(key, string(bounded))
}

func summarizeValue(value interface{}) string {
	summary := map[string]interface{}{"type": fmt.Sprintf("%T", value)}
	switch typed := value.(type) {
	case string:
		summary["bytes"] = len(typed)
		summary["runes"] = utf8.RuneCountInString(typed)
	case []byte:
		summary["bytes"] = len(typed)
	default:
		reflected := reflect.ValueOf(value)
		if reflected.IsValid() {
			switch reflected.Kind() {
			case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
				summary["length"] = reflected.Len()
			}
		}
	}
	encoded, _ := json.Marshal(summary)
	return string(encoded)
}

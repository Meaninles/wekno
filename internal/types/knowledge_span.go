package types

import "time"

// Span kinds — kept narrow because every kind has dedicated rendering on
// the frontend timeline:
//
//   - SpanKindRoot     — the per-(knowledge, attempt) trace root. Always
//     the parent_span_id ancestor of every other span
//     in that attempt. UI uses it for total elapsed.
//   - SpanKindStage    — one of the 5 canonical stages (DocReader, etc.).
//     UI renders these as the timeline segments.
//   - SpanKindSubSpan  — anything inside a stage (e.g. multimodal.image[i]).
//     UI shows them as collapsible children.
//   - SpanKindGeneration — a SubSpan that wraps an LLM/VLM call. Same UI
//     treatment as SubSpan but tagged so we can stitch
//     to the matching Langfuse generation.
const (
	SpanKindRoot       = "root"
	SpanKindStage      = "stage"
	SpanKindSubSpan    = "subspan"
	SpanKindGeneration = "generation"
)

// Span statuses. We deliberately distinguish "failed" (this span itself
// errored) from "cancelled" (an upstream span failed and we abandoned this
// one without running it) so the UI can render the cause differently —
// "you broke X, so we never ran Y" vs. "Y itself broke".
const (
	SpanStatusPending   = "pending"
	SpanStatusRunning   = "running"
	SpanStatusDone      = "done"
	SpanStatusFailed    = "failed"
	SpanStatusSkipped   = "skipped"   // intentionally not run (e.g. multimodal on a text-only doc)
	SpanStatusCancelled = "cancelled" // not run because an upstream span failed
)

// Stage names — the closed set the UI builds its 5-segment timeline from.
// Adding a stage requires a coordinated frontend release. SubSpan names
// are free-form (e.g. "multimodal.image[0]") and don't go through this
// list.
const (
	StageDocReader   = "docreader"
	StageChunking    = "chunking"
	StageEmbedding   = "embedding"
	StageMultimodal  = "multimodal"
	StagePostProcess = "postprocess"
)

// AllStages is the canonical, ordered stage list. Used by the API layer
// to synthesize "pending" placeholders so the timeline always renders five
// segments even before parsing starts.
var AllStages = []string{
	StageDocReader,
	StageChunking,
	StageEmbedding,
	StageMultimodal,
	StagePostProcess,
}

// StageDependencies declares the DAG between stages. Used by the tracker
// to cascade-cancel dependents when a stage fails — a Chunking failure
// silently turns Embedding/Multimodal/PostProcess into "cancelled" so the
// timeline shows a clear blast radius instead of three pending spinners.
//
// Important: Multimodal does NOT depend on Embedding. They share Chunking
// as their upstream and are otherwise independent (Multimodal kicks off
// regardless of vector indexing config). PostProcess joins both before
// running its handlers.
var StageDependencies = map[string][]string{
	StageDocReader:   nil,
	StageChunking:    {StageDocReader},
	StageEmbedding:   {StageChunking},
	StageMultimodal:  {StageChunking},
	StagePostProcess: {StageEmbedding, StageMultimodal},
}

// KnowledgeProcessingSpan is the API/service DTO for one logical processing
// span. Persistence is owned exclusively by custom processingtrace V2;
// keeping this projection storage-agnostic prevents accidental recreation of
// the removed physical-delivery table. ErrorDetail remains admin-only.
type KnowledgeProcessingSpan struct {
	ID           int64      `json:"-"`
	KnowledgeID  string     `json:"knowledge_id"`
	Attempt      int        `json:"attempt"`
	SpanID       string     `json:"span_id"`
	ParentSpanID string     `json:"parent_span_id,omitempty"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Status       string     `json:"status"`
	Input        JSONMap    `json:"input,omitempty"`
	Output       JSONMap    `json:"output,omitempty"`
	Metadata     JSONMap    `json:"metadata,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	ErrorDetail  string     `json:"-"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	DurationMs   int64      `json:"duration_ms,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// SpanTreeNode is the API-only tree projection. The repo returns flat
// rows; the handler/tracker assembles SpanTreeNode for the response.
type SpanTreeNode struct {
	KnowledgeProcessingSpan
	Children []*SpanTreeNode `json:"children,omitempty"`
}

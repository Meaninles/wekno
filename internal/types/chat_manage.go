package types

import (
	"maps"
	"strings"
)

// PipelineRequest holds immutable configuration set once at the request entry point.
type PipelineRequest struct {
	SessionID    string `json:"session_id"`
	UserID       string `json:"user_id"`
	Query        string `json:"query,omitempty"`
	EnableMemory bool   `json:"enable_memory"`
	MaxRounds    int    `json:"max_rounds"`

	// Knowledge base retrieval parameters
	KnowledgeBaseIDs []string      `json:"knowledge_base_ids"`
	KnowledgeIDs     []string      `json:"knowledge_ids,omitempty"`
	SearchTargets    SearchTargets `json:"-"`
	VectorThreshold  float64       `json:"vector_threshold"`
	KeywordThreshold float64       `json:"keyword_threshold"`
	EmbeddingTopK    int           `json:"embedding_top_k"`
	VectorDatabase   string        `json:"vector_database"`

	// Rerank parameters
	RerankModelID   string  `json:"rerank_model_id"`
	RerankTopK      int     `json:"rerank_top_k"`
	RerankThreshold float64 `json:"rerank_threshold"`

	// Chat model parameters
	ChatModelID      string           `json:"chat_model_id"`
	SummaryConfig    SummaryConfig    `json:"summary_config"`
	FallbackStrategy FallbackStrategy `json:"fallback_strategy"`
	FallbackResponse string           `json:"fallback_response"`
	FallbackPrompt   string           `json:"fallback_prompt"`

	// Rewrite parameters
	EnableRewrite        bool   `json:"enable_rewrite"`
	EnableQueryExpansion bool   `json:"enable_query_expansion"`
	RewritePromptSystem  string `json:"rewrite_prompt_system"`
	RewritePromptUser    string `json:"rewrite_prompt_user"`
	// QueryUnderstandModelID, when set, overrides the chat model used for
	// the query-understanding (rewrite + intent classification) stage only.
	// Empty means fall back to ChatModelID.
	QueryUnderstandModelID string `json:"query_understand_model_id,omitempty"`

	// FAQ strategy
	FAQPriorityEnabled       bool    `json:"-"`
	FAQDirectAnswerThreshold float64 `json:"-"`
	FAQScoreBoost            float64 `json:"-"`

	// DataAnalysisEnabled controls whether the in-pipeline DuckDB SQL
	// tabular SQL stage runs. Off by default to avoid an extra LLM call on
	// every RAG request that happens to retrieve CSV/Excel chunks.
	DataAnalysisEnabled bool `json:"-"`

	// Image / multimodal support
	Images                  []string `json:"-"`
	VLMModelID              string   `json:"-"`
	ChatModelSupportsVision bool     `json:"-"`

	// File attachments support
	Attachments MessageAttachments `json:"-"`

	// IntentPromptOverrides holds agent-level intent prompt overrides for the
	// query-understanding stage. Empty values fall back to tenant/global defaults.
	IntentPromptOverrides map[string]string `json:"-"`

	// Misc request-scoped config
	TenantID                uint64 `json:"-"`
	WebSearchEnabled        bool   `json:"-"`
	WebSearchProviderID     string `json:"-"` // Resolved from agent config or tenant default
	WebSearchMaxResults     int    `json:"-"` // Resolved from agent config or tenant default
	WebFetchEnabled         bool   `json:"-"` // Auto-fetch full page content for web search results after rerank
	WebFetchTopN            int    `json:"-"` // Max pages to fetch (default 3)
	Language                string `json:"-"`
	LightweightSkillContext string `json:"-"`
}

// QueryIntent represents the classified intent of a user query.
type QueryIntent string

const (
	IntentKBSearch      QueryIntent = "kb_search"
	IntentWebSearch     QueryIntent = "web_search"
	IntentGreeting      QueryIntent = "greeting"
	IntentChitchat      QueryIntent = "chitchat"
	IntentFollowUp      QueryIntent = "follow_up"
	IntentConversation  QueryIntent = "conversation_state"
	IntentImageOnly     QueryIntent = "image_only"
	IntentDocOnly       QueryIntent = "doc_only"
	IntentSummarize     QueryIntent = "summarize"
	IntentClarification QueryIntent = "clarification"
)

// EvidenceNeed records whether the current turn needs a source outside the
// user-authored dialogue. It is deliberately independent from QueryIntent: a
// single request may update dialogue state and also ask for fresh evidence.
// The empty value preserves the legacy intent-only routing contract for custom
// rewrite prompts that have not adopted this field yet.
type EvidenceNeed string

const (
	EvidenceNeedNone          EvidenceNeed = "none"
	EvidenceNeedKnowledgeBase EvidenceNeed = "knowledge_base"
	EvidenceNeedWeb           EvidenceNeed = "web"
)

// NeedsKBRetrieval returns true when the intent requires knowledge base search.
// The zero value (empty string) is treated as needing retrieval for safety.
// Note: IntentWebSearch is NOT included — use ChatManage.NeedsRetrieval()
// which also considers the WebSearchEnabled flag.
func (i QueryIntent) NeedsKBRetrieval() bool {
	switch i {
	case IntentKBSearch, IntentClarification, "":
		return true
	default:
		return false
	}
}

// PipelineState holds mutable intermediate data that plugins read and write
// as the pipeline progresses.
type PipelineState struct {
	RewriteQuery       string       `json:"rewrite_query,omitempty"`
	Intent             QueryIntent  `json:"intent,omitempty"`
	EvidenceNeed       EvidenceNeed `json:"evidence_need,omitempty"`
	History            []*History   `json:"history,omitempty"`
	DurableUserContext string       `json:"-"`

	SearchResult []*SearchResult `json:"-"`
	RerankResult []*SearchResult `json:"-"`
	MergeResult  []*SearchResult `json:"-"`
	// CitationResult contains exact, independently citable evidence units. It is
	// intentionally separate from MergeResult, whose Content may contain parent,
	// neighbor or overlap-expanded model context.
	CitationResult       []*SearchResult   `json:"-"`
	Entity               []string          `json:"-"`
	EntityKBIDs          []string          `json:"-"`
	EntityKnowledge      map[string]string `json:"-"`
	GraphResult          *GraphData        `json:"-"`
	UserContent          string            `json:"-"`
	RenderedContexts     string            `json:"-"`
	ChatResponse         *ChatResponse     `json:"-"`
	ImageDescription     string            `json:"-"`
	QuotedContext        string            `json:"-"` // Quoted message text, injected at LLM prompt stage
	SystemPromptOverride string            `json:"-"`
}

// PipelineContext holds runtime context for the current pipeline execution.
type PipelineContext struct {
	EventBus      EventBusInterface `json:"-"`
	MessageID     string            `json:"-"`
	UserMessageID string            `json:"-"`
}

// ChatManage represents the full configuration, state and runtime context
// for a chat pipeline execution. It embeds PipelineRequest (immutable config),
// PipelineState (mutable intermediate data), and PipelineContext (runtime handles).
type ChatManage struct {
	PipelineRequest
	PipelineState
	PipelineContext
}

// HasKnowledgeTargets reports whether this request has any concrete knowledge
// source to search. An intent alone must not manufacture a retrieval operation:
// agents with no selected/all-mode KB scope should answer directly or explain
// that evidence is unavailable without emitting an empty knowledge-search run.
func (c *ChatManage) HasKnowledgeTargets() bool {
	if c == nil {
		return false
	}
	for _, target := range c.SearchTargets {
		if target == nil {
			continue
		}
		if strings.TrimSpace(target.KnowledgeBaseID) != "" {
			return true
		}
		for _, knowledgeID := range target.KnowledgeIDs {
			if strings.TrimSpace(knowledgeID) != "" {
				return true
			}
		}
	}
	for _, knowledgeBaseID := range c.KnowledgeBaseIDs {
		if strings.TrimSpace(knowledgeBaseID) != "" {
			return true
		}
	}
	for _, knowledgeID := range c.KnowledgeIDs {
		if strings.TrimSpace(knowledgeID) != "" {
			return true
		}
	}
	return false
}

// NeedsRetrieval returns true when the current pipeline execution should run
// retrieval stages (search, rerank, merge, etc.) and the corresponding source
// is actually available. Web intent never silently falls back to a KB, and a
// KB/clarification intent with no concrete KB target never emits an empty
// knowledge-search lifecycle.
func (c *ChatManage) NeedsRetrieval() bool {
	if c == nil {
		return false
	}
	// Explicit retrieval intents remain authoritative for compatibility and
	// fail-safe behavior. EvidenceNeed adds the previously unrepresentable mixed
	// case; it never suppresses retrieval requested by an existing intent.
	if c.Intent == IntentWebSearch {
		return c.WebSearchEnabled
	}
	if c.Intent.NeedsKBRetrieval() {
		return c.HasKnowledgeTargets()
	}
	switch c.EvidenceNeed {
	case EvidenceNeedKnowledgeBase:
		return c.HasKnowledgeTargets()
	case EvidenceNeedWeb:
		return c.WebSearchEnabled
	}
	return false
}

// Clone creates a deep copy of the ChatManage object.
// PipelineContext fields (EventBus, MessageID, etc.) are NOT copied because they
// are per-execution handles that should not be shared across clones.
func (c *ChatManage) Clone() *ChatManage {
	knowledgeBaseIDs := make([]string, len(c.KnowledgeBaseIDs))
	copy(knowledgeBaseIDs, c.KnowledgeBaseIDs)

	knowledgeIDs := make([]string, len(c.KnowledgeIDs))
	copy(knowledgeIDs, c.KnowledgeIDs)

	searchTargets := make(SearchTargets, len(c.SearchTargets))
	for i, t := range c.SearchTargets {
		if t != nil {
			kidsCopy := make([]string, len(t.KnowledgeIDs))
			copy(kidsCopy, t.KnowledgeIDs)
			tagIDsCopy := make([]string, len(t.TagIDs))
			copy(tagIDsCopy, t.TagIDs)
			searchTargets[i] = &SearchTarget{
				Type:            t.Type,
				KnowledgeBaseID: t.KnowledgeBaseID,
				TenantID:        t.TenantID,
				KnowledgeIDs:    kidsCopy,
				TagIDs:          tagIDsCopy,
			}
		}
	}

	// Deep copy Entity using in search entity plugin
	entity := make([]string, len(c.Entity))
	copy(entity, c.Entity)

	entityKBIDs := make([]string, len(c.EntityKBIDs))
	copy(entityKBIDs, c.EntityKBIDs)

	entityKnowledge := make(map[string]string)
	maps.Copy(entityKnowledge, c.EntityKnowledge)

	return &ChatManage{
		PipelineRequest: PipelineRequest{
			Query:                    c.Query,
			SessionID:                c.SessionID,
			UserID:                   c.UserID,
			EnableMemory:             c.EnableMemory,
			MaxRounds:                c.MaxRounds,
			KnowledgeBaseIDs:         knowledgeBaseIDs,
			KnowledgeIDs:             knowledgeIDs,
			SearchTargets:            searchTargets,
			VectorThreshold:          c.VectorThreshold,
			KeywordThreshold:         c.KeywordThreshold,
			EmbeddingTopK:            c.EmbeddingTopK,
			VectorDatabase:           c.VectorDatabase,
			RerankModelID:            c.RerankModelID,
			RerankTopK:               c.RerankTopK,
			RerankThreshold:          c.RerankThreshold,
			ChatModelID:              c.ChatModelID,
			SummaryConfig:            c.SummaryConfig,
			FallbackStrategy:         c.FallbackStrategy,
			FallbackResponse:         c.FallbackResponse,
			FallbackPrompt:           c.FallbackPrompt,
			EnableRewrite:            c.EnableRewrite,
			EnableQueryExpansion:     c.EnableQueryExpansion,
			RewritePromptSystem:      c.RewritePromptSystem,
			RewritePromptUser:        c.RewritePromptUser,
			QueryUnderstandModelID:   c.QueryUnderstandModelID,
			FAQPriorityEnabled:       c.FAQPriorityEnabled,
			FAQDirectAnswerThreshold: c.FAQDirectAnswerThreshold,
			FAQScoreBoost:            c.FAQScoreBoost,
			DataAnalysisEnabled:      c.DataAnalysisEnabled,
			Images:                   append([]string(nil), c.Images...),
			VLMModelID:               c.VLMModelID,
			ChatModelSupportsVision:  c.ChatModelSupportsVision,
			Attachments:              append(MessageAttachments(nil), c.Attachments...),
			TenantID:                 c.TenantID,
			WebSearchEnabled:         c.WebSearchEnabled,
			WebSearchProviderID:      c.WebSearchProviderID,
			WebSearchMaxResults:      c.WebSearchMaxResults,
			WebFetchEnabled:          c.WebFetchEnabled,
			WebFetchTopN:             c.WebFetchTopN,
			Language:                 c.Language,
			LightweightSkillContext:  c.LightweightSkillContext,
			IntentPromptOverrides:    maps.Clone(c.IntentPromptOverrides),
		},
		PipelineState: PipelineState{
			RewriteQuery:         c.RewriteQuery,
			Intent:               c.Intent,
			EvidenceNeed:         c.EvidenceNeed,
			DurableUserContext:   c.DurableUserContext,
			ImageDescription:     c.ImageDescription,
			QuotedContext:        c.QuotedContext,
			SystemPromptOverride: c.SystemPromptOverride,
			RenderedContexts:     c.RenderedContexts,
			Entity:               entity,
			EntityKBIDs:          entityKBIDs,
			EntityKnowledge:      entityKnowledge,
		},
	}
}

// EventType represents different stages in the RAG (Retrieval Augmented Generation) pipeline
type EventType string

const (
	LOAD_HISTORY           EventType = "load_history"
	QUERY_UNDERSTAND       EventType = "query_understand"
	CHUNK_SEARCH           EventType = "chunk_search"
	CHUNK_SEARCH_PARALLEL  EventType = "chunk_search_parallel"
	ENTITY_SEARCH          EventType = "entity_search"
	CHUNK_RERANK           EventType = "chunk_rerank"
	WEB_FETCH              EventType = "web_fetch"
	CHUNK_MERGE            EventType = "chunk_merge"
	DATA_ANALYSIS          EventType = "data_analysis"
	INTO_CHAT_MESSAGE      EventType = "into_chat_message"
	CHAT_COMPLETION        EventType = "chat_completion"
	CHAT_COMPLETION_STREAM EventType = "chat_completion_stream"
	FILTER_TOP_K           EventType = "filter_top_k"
	MEMORY_RETRIEVAL       EventType = "memory_retrieval"
	MEMORY_STORAGE         EventType = "memory_storage"
)

// PipelineBuilder dynamically assembles a pipeline as an ordered list of EventTypes.
type PipelineBuilder struct {
	stages []EventType
}

// NewPipelineBuilder returns an empty builder.
func NewPipelineBuilder() *PipelineBuilder {
	return &PipelineBuilder{}
}

// Add appends one or more stages unconditionally.
func (b *PipelineBuilder) Add(stages ...EventType) *PipelineBuilder {
	b.stages = append(b.stages, stages...)
	return b
}

// AddIf appends stages only when the condition is true.
func (b *PipelineBuilder) AddIf(cond bool, stages ...EventType) *PipelineBuilder {
	if cond {
		b.stages = append(b.stages, stages...)
	}
	return b
}

// Build returns the final event list.  The builder must not be reused.
func (b *PipelineBuilder) Build() []EventType {
	out := make([]EventType, len(b.stages))
	copy(out, b.stages)
	return out
}

// Pipeline defines the sequence of events for different chat modes.
// Kept as a convenience lookup for callers that don't need dynamic composition.
var Pipeline = map[string][]EventType{
	"chat": {
		CHAT_COMPLETION,
	},
	"chat_stream": {
		CHAT_COMPLETION_STREAM,
	},
	"chat_history_stream": {
		LOAD_HISTORY,
		MEMORY_RETRIEVAL,
		CHAT_COMPLETION_STREAM,
		MEMORY_STORAGE,
	},
	"rag": {
		CHUNK_SEARCH,
		CHUNK_RERANK,
		CHUNK_MERGE,
		INTO_CHAT_MESSAGE,
		CHAT_COMPLETION,
	},
	"rag_stream": {
		LOAD_HISTORY,
		QUERY_UNDERSTAND,
		CHUNK_SEARCH_PARALLEL,
		CHUNK_RERANK,
		CHUNK_MERGE,
		FILTER_TOP_K,
		DATA_ANALYSIS,
		INTO_CHAT_MESSAGE,
		CHAT_COMPLETION_STREAM,
	},
}

// Pipline is a deprecated alias for Pipeline (kept for backward compatibility).
var Pipline = Pipeline

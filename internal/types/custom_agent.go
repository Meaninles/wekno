package types

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// All conversations run in one harness with two fixed execution budgets.
const (
	BuiltinDeepResearcherID          = "builtin-deep-researcher"
	BuiltinDataAnalystID             = "builtin-data-analyst"
	BuiltinTableAnalystID            = "builtin-table-analyst"
	BuiltinKnowledgeGraphExpertID    = "builtin-knowledge-graph-expert"
	BuiltinDocumentProcessingID      = "builtin-document-processing"
	BuiltinGeneralAgentID            = "builtin-general-agent"
	BuiltinWikiResearcherID          = "builtin-wiki-researcher"
	BuiltinWikiFixerID               = "builtin-wiki-fixer"
	AgentTypeWikiQA                  = "wiki-qa"
	AgentTypeHybridRAGWiki           = "hybrid-rag-wiki"
	AgentTypeDataAnalysis            = "data-analysis"
	AgentTypeTableAnalysis           = "table-analysis"
	AgentTypeDocumentProcessingAgent = "document-processing-agent"
	AgentTypeGeneralAgent            = "general-agent"
	AgentTypeKnowledgeBaseManager    = "knowledge-base-manager"
	AgentTypeCustom                  = "custom"
	BuiltinKnowledgeQAID             = "builtin-knowledge-qa"
	AgentModeUnified                 = "agent"
	AgentTypeKnowledgeQA             = "knowledge-qa"
)

func AgentIterationBudget(agentType string) int {
	if agentType == AgentTypeKnowledgeQA {
		return 15
	}
	return 50
}

func HasWorkspaceCapabilities(agentType string) bool {
	return agentType == AgentTypeGeneralAgent || agentType == AgentTypeDataAnalysis ||
		agentType == AgentTypeTableAnalysis || agentType == AgentTypeDocumentProcessingAgent ||
		agentType == AgentTypeKnowledgeBaseManager
}

func IsKnownAgentType(agentType string) bool {
	switch agentType {
	case AgentTypeKnowledgeQA, AgentTypeGeneralAgent, AgentTypeDataAnalysis, AgentTypeTableAnalysis,
		AgentTypeDocumentProcessingAgent, AgentTypeKnowledgeBaseManager, AgentTypeWikiQA, AgentTypeHybridRAGWiki, AgentTypeCustom:
		return true
	}
	return false
}

// CustomAgent represents a configurable AI agent (similar to GPTs)
type CustomAgent struct {
	// Unique identifier of the agent (composite primary key with TenantID)
	// Built-in IDs identify distinct capability profiles.
	// For custom agents, this is a UUID
	ID string `yaml:"id" json:"id" gorm:"type:varchar(36);primaryKey"`
	// Name of the agent
	Name string `yaml:"name" json:"name" gorm:"type:varchar(255);not null"`
	// Description of the agent
	Description string `yaml:"description" json:"description" gorm:"type:text"`
	// Avatar/Icon of the agent (emoji or icon name)
	Avatar string `yaml:"avatar" json:"avatar" gorm:"type:varchar(64)"`
	// Whether this is a built-in agent (normal mode / agent mode)
	IsBuiltin bool `yaml:"is_builtin" json:"is_builtin" gorm:"default:false"`
	// Tenant ID (composite primary key with ID)
	TenantID uint64 `yaml:"tenant_id" json:"tenant_id" gorm:"primaryKey"`
	// Created by user ID
	CreatedBy string `yaml:"created_by" json:"created_by" gorm:"type:varchar(36)"`
	// RunnableByViewer controls whether users with TenantRoleViewer can run this
	// agent in agent mode. Plain RAG / Wiki QA sessions are unaffected.
	RunnableByViewer bool `yaml:"runnable_by_viewer" json:"runnable_by_viewer"`

	// Agent configuration
	Config CustomAgentConfig `yaml:"config" json:"config" gorm:"type:json"`

	// Timestamps
	CreatedAt time.Time      `yaml:"created_at" json:"created_at"`
	UpdatedAt time.Time      `yaml:"updated_at" json:"updated_at"`
	DeletedAt gorm.DeletedAt `yaml:"deleted_at" json:"deleted_at" gorm:"index"`

	// CreatorName 由 list handler 在返回前批量回填，作用同 KnowledgeBase.CreatorName：
	// 让前端列表卡片区分「我创建」与「同租户其他成员创建」。不落库，内建 agent / 老数据
	// 仍可能为空。
	CreatorName string `yaml:"-" json:"creator_name,omitempty" gorm:"-"`
}

// CustomAgentConfig represents the configuration of a custom agent
type CustomAgentConfig struct {
	RetrievalBudget RetrievalBudget `json:"retrieval_budget" yaml:"retrieval_budget"`
	// ===== Basic Settings =====
	// AgentMode is always "agent": every profile uses the same harness.
	AgentMode string `yaml:"agent_mode" json:"agent_mode"`
	// AgentType is a distinct capability profile that pre-fills
	// system prompt, allowed tools and recommended KB compatibility.
	// Supported profiles are declared by the AgentType constants.
	// API validation rejects unknown profile types.
	AgentType string `yaml:"agent_type" json:"agent_type,omitempty"`
	// System prompt for the agent (unified prompt, uses web_search_status placeholder for dynamic behavior)
	SystemPrompt string `yaml:"system_prompt" json:"system_prompt"`
	// SystemPromptID references a template ID in prompt_templates/ YAML files.
	// If set and SystemPrompt is empty, the template content will be resolved at startup.
	SystemPromptID string `yaml:"system_prompt_id" json:"system_prompt_id,omitempty"`
	// DocumentTemplate configures fixed template requirement/reference files for
	// the AgentScope document-processing agent. Nil means "use built-in defaults"
	// only for AgentTypeGeneralAgent; an explicit empty value means
	// the user intentionally removed all defaults.
	DocumentTemplate *DocumentTemplateConfig `yaml:"document_template" json:"document_template,omitempty"`
	// KnowledgeManagement configures the selected-KB mutation ceiling for the
	// knowledge-base-manager type. Runtime tools still intersect it with the
	// current turn selection and the caller's live platform permissions.
	KnowledgeManagement *KnowledgeManagementConfig `yaml:"knowledge_management" json:"knowledge_management,omitempty"`

	// ===== Model Settings =====
	// Model ID to use for conversations
	ModelID string `yaml:"model_id" json:"model_id"`
	// ReRank model ID for retrieval
	RerankModelID string `yaml:"rerank_model_id" json:"rerank_model_id"`
	// Temperature for LLM (0-1)
	Temperature float64 `yaml:"temperature" json:"temperature"`
	// Maximum completion tokens (only for normal mode)
	MaxCompletionTokens int `yaml:"max_completion_tokens" json:"max_completion_tokens"`
	// Whether to enable thinking mode (for models that support extended thinking)
	Thinking *bool `yaml:"thinking" json:"thinking"`
	// Whether the agent runtime may create downloadable artifacts.
	EnableArtifacts bool `yaml:"enable_artifacts" json:"enable_artifacts"`
	// ===== Agent Mode Settings =====
	// Maximum iterations for ReAct loop (only for agent type)
	MaxIterations int `yaml:"max_iterations" json:"max_iterations"`
	// Timeout for a single LLM call in seconds (0 = use global default)
	LLMCallTimeout int `yaml:"llm_call_timeout" json:"llm_call_timeout,omitempty"`
	// Allowed tools (only for agent type)
	AllowedTools []string `yaml:"allowed_tools" json:"allowed_tools"`
	// MCP service selection mode: "all" = all enabled MCP services, "selected" = specific services, "none" = no MCP
	MCPSelectionMode string `yaml:"mcp_selection_mode" json:"mcp_selection_mode"`
	// Selected MCP service IDs (only used when MCPSelectionMode is "selected")
	MCPServices []string `yaml:"mcp_services" json:"mcp_services"`
	// MCPAuthWaitTimeout is how many seconds to wait for in-conversation OAuth
	// authorization before skipping. <=0 uses the gate's configured timeout.
	MCPAuthWaitTimeout int `yaml:"mcp_auth_wait_timeout,omitempty" json:"mcp_auth_wait_timeout,omitempty"`

	// ===== Skills Settings =====
	// Deprecated: legacy skills are now treated as lightweight prompt skills.
	// Skills selection mode: "all" = all lightweight skills, "selected" = specific skills, "none" = no skills
	SkillsSelectionMode string `yaml:"skills_selection_mode" json:"skills_selection_mode"`
	// Deprecated: use SelectedLightweightSkills.
	SelectedSkills []string `yaml:"selected_skills" json:"selected_skills"`
	// Lightweight skills are prompt/context snippets. They can be configured on
	// agents and temporarily selected in chat.
	LightweightSkillsSelectionMode string   `yaml:"lightweight_skills_selection_mode" json:"lightweight_skills_selection_mode,omitempty"`
	SelectedLightweightSkills      []string `yaml:"selected_lightweight_skills" json:"selected_lightweight_skills,omitempty"`
	// Professional skills are real AgentScope skills materialized as
	// /workspace/skills/<name>/SKILL.md for AgentScope based agents.
	ProfessionalSkillsSelectionMode string   `yaml:"professional_skills_selection_mode" json:"professional_skills_selection_mode,omitempty"`
	SelectedProfessionalSkills      []string `yaml:"selected_professional_skills" json:"selected_professional_skills,omitempty"`
	// ===== Knowledge Base Settings =====
	// Knowledge base selection mode: "all" = all KBs, "selected" = specific KBs, "none" = no KB
	KBSelectionMode string `yaml:"kb_selection_mode" json:"kb_selection_mode"`
	// Associated knowledge base IDs (only used when KBSelectionMode is "selected")
	KnowledgeBases []string `yaml:"knowledge_bases" json:"knowledge_bases"`
	// Associated database analytics source IDs for data-analysis agents.
	DBDataSources []string `yaml:"db_data_sources" json:"db_data_sources,omitempty"`
	// Whether to retrieve knowledge base only when explicitly mentioned with @ (default: false)
	// When true, knowledge base retrieval only happens if user explicitly mentions KB/files with @
	// When false, knowledge base retrieval happens according to KBSelectionMode
	RetrieveKBOnlyWhenMentioned bool `yaml:"retrieve_kb_only_when_mentioned" json:"retrieve_kb_only_when_mentioned"`

	// Whether to retain retrieval history across turns
	RetainRetrievalHistory bool `yaml:"retain_retrieval_history" json:"retain_retrieval_history"`

	// ===== Image Upload / Multimodal Settings =====
	// Whether image upload is enabled for this agent (default: false)
	ImageUploadEnabled bool `yaml:"image_upload_enabled" json:"image_upload_enabled"`
	// VLM model ID for image analysis (optional, falls back to tenant-level VLM)
	VLMModelID string `yaml:"vlm_model_id" json:"vlm_model_id"`
	// Whether audio upload (ASR transcription) is enabled for this agent (default: false)
	AudioUploadEnabled bool `yaml:"audio_upload_enabled" json:"audio_upload_enabled"`
	// ASR model ID for audio transcription (optional)
	ASRModelID string `yaml:"asr_model_id" json:"asr_model_id"`
	// Storage provider for image uploads: "local", "minio", "cos", "tos", "s3", "oss", "ks3".
	// Empty means use the global/tenant default provider.
	ImageStorageProvider string `yaml:"image_storage_provider" json:"image_storage_provider"`

	// ===== File Type Restriction Settings =====
	// Supported file types for this agent (e.g., ["csv", "xlsx", "xls"])
	// Empty means all file types are supported
	// When set, only files with matching extensions can be used with this agent
	SupportedFileTypes []string `yaml:"supported_file_types" json:"supported_file_types"`

	// ===== FAQ Strategy Settings =====
	// Whether FAQ priority strategy is enabled (FAQ answers prioritized over document chunks)
	FAQPriorityEnabled bool `yaml:"faq_priority_enabled" json:"faq_priority_enabled"`
	// FAQ direct answer threshold - if similarity > this value, use FAQ answer directly
	FAQDirectAnswerThreshold float64 `yaml:"faq_direct_answer_threshold" json:"faq_direct_answer_threshold"`

	// ===== Web Search Settings =====
	// Whether web search is enabled
	WebSearchEnabled bool `yaml:"web_search_enabled" json:"web_search_enabled"`
	// Maximum web search results
	WebSearchMaxResults int `yaml:"web_search_max_results" json:"web_search_max_results"`
	// WebSearchProviderID references a specific WebSearchProviderEntity.
	// If empty, the tenant's default provider (is_default=true) is used.
	WebSearchProviderID string `yaml:"web_search_provider_id" json:"web_search_provider_id,omitempty"`
	// Whether to auto-fetch full page content for reranked web search results
	WebFetchEnabled bool `yaml:"web_fetch_enabled" json:"web_fetch_enabled"`
	// Max number of pages to fetch after rerank (default: 3)
	WebFetchTopN int `yaml:"web_fetch_top_n" json:"web_fetch_top_n,omitempty"`

	// ===== Multi-turn Conversation Settings =====
	// Whether multi-turn conversation is enabled
	MultiTurnEnabled bool `yaml:"multi_turn_enabled" json:"multi_turn_enabled"`
	// Number of history turns to keep in context
	HistoryTurns int `yaml:"history_turns" json:"history_turns"`

	// ===== Retrieval Strategy Settings (for both modes) =====
	// Embedding/Vector retrieval top K
	EmbeddingTopK int `yaml:"embedding_top_k" json:"embedding_top_k"`
	// Keyword retrieval threshold
	KeywordThreshold float64 `yaml:"keyword_threshold" json:"keyword_threshold"`
	// Vector retrieval threshold
	VectorThreshold float64 `yaml:"vector_threshold" json:"vector_threshold"`
	// Rerank top K
	RerankTopK int `yaml:"rerank_top_k" json:"rerank_top_k"`
	// Rerank threshold
	RerankThreshold float64 `yaml:"rerank_threshold" json:"rerank_threshold"`

	// ===== Suggested Prompts =====
	// 推荐问题列表，用于在前端对话面板展示快捷提问
	SuggestedPrompts []string `yaml:"suggested_prompts" json:"suggested_prompts,omitempty"`
}

func defaultCustomAgentConfig() CustomAgentConfig {
	return CustomAgentConfig{
		ImageUploadEnabled:  true,
		AudioUploadEnabled:  true,
		Temperature:         0.7,
		MaxIterations:       50,
		WebSearchMaxResults: 5,
		HistoryTurns:        5,
		EmbeddingTopK:       10,
		KeywordThreshold:    0.3,
		VectorThreshold:     0.5,
		RerankTopK:          5,
		MaxCompletionTokens: 16384,
	}
}

// UnmarshalJSON applies defaults only for fields omitted from JSON.
// Numeric thresholds legitimately accept 0 as "no threshold"; they must not be
// overwritten later just because Go's zero value is also 0.
func (c *CustomAgentConfig) UnmarshalJSON(data []byte) error {
	type alias CustomAgentConfig
	cfg := defaultCustomAgentConfig()
	if err := json.Unmarshal(data, (*alias)(&cfg)); err != nil {
		return err
	}
	*c = cfg
	return nil
}

// Value implements driver.Valuer interface for CustomAgentConfig
func (c CustomAgentConfig) Value() (driver.Value, error) {
	return json.Marshal(c)
}

// Scan implements sql.Scanner interface for CustomAgentConfig
func (c *CustomAgentConfig) Scan(value interface{}) error {
	if value == nil {
		*c = defaultCustomAgentConfig()
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return nil
	}
	return json.Unmarshal(b, c)
}

// TableName returns the table name for CustomAgent
func (CustomAgent) TableName() string {
	return "custom_agents"
}

// EnsureDefaults sets default values for the agent
func (a *CustomAgent) EnsureDefaults() {
	if a == nil {
		return
	}
	if a.Config.Temperature < 0 {
		a.Config.Temperature = 0.7
	}
	a.Config.AgentMode = AgentModeUnified
	if a.Config.AgentType == "" {
		a.Config.AgentType = AgentTypeGeneralAgent
	}
	a.Config.MaxIterations = AgentIterationBudget(a.Config.AgentType)
	if a.Config.AgentType == AgentTypeDataAnalysis || a.Config.AgentType == AgentTypeTableAnalysis || a.Config.AgentType == AgentTypeDocumentProcessingAgent {
		a.Config.EnableArtifacts = true
	}
	if a.Config.WebSearchMaxResults == 0 {
		a.Config.WebSearchMaxResults = 5
	}
	if a.Config.HistoryTurns == 0 {
		a.Config.HistoryTurns = 5
	}
	// Retrieval strategy defaults
	if a.Config.EmbeddingTopK == 0 {
		a.Config.EmbeddingTopK = 10
	}
	if a.Config.KeywordThreshold < 0 {
		a.Config.KeywordThreshold = 0.3
	}
	if a.Config.VectorThreshold < 0 {
		a.Config.VectorThreshold = 0.5
	}
	if a.Config.RerankTopK == 0 {
		a.Config.RerankTopK = 5
	}
	if a.Config.MaxCompletionTokens == 0 {
		a.Config.MaxCompletionTokens = 16384
	}
	// Agent mode should always enable multi-turn conversation
	if a.Config.AgentMode == AgentModeUnified {
		a.Config.MultiTurnEnabled = true
	}
	// An unset capability defers to the model/provider default. Preserve an
	// explicit user choice, including false, across every runtime adapter.
	_ = NormalizeCustomAgentDocumentTemplateConfig(&a.Config)
}

// IsAgentMode returns true if this agent uses ReAct agent mode
func (a *CustomAgent) IsAgentMode() bool {
	return true
}

// SuggestedQuestion 推荐问题
type SuggestedQuestion struct {
	// 问题文本
	Question string `json:"question"`
	// 来源类型: "agent_config", "faq", "document", "wiki"
	Source string `json:"source"`
	// 来源知识库ID（仅 faq/document/wiki 来源时有值）
	KnowledgeBaseID string `json:"knowledge_base_id,omitempty"`
}

// BuiltinAgentRegistry provides a registry of all built-in agents.
// It is initialised empty and populated by LoadBuiltinAgentsConfig from
// config/builtin_agents.yaml at startup via rebuildRegistryFromConfig.
var BuiltinAgentRegistry = map[string]func(uint64) *CustomAgent{}

var builtinAgentIDsOrdered = []string{
	BuiltinKnowledgeQAID,
	BuiltinWikiResearcherID,
	BuiltinDeepResearcherID,
	BuiltinDataAnalystID,
	BuiltinTableAnalystID,
	BuiltinGeneralAgentID,
	BuiltinDocumentProcessingID,
	BuiltinKnowledgeGraphExpertID,
}

// GetBuiltinAgentIDs returns all built-in agent IDs in fixed order
func GetBuiltinAgentIDs() []string {
	return builtinAgentIDsOrdered
}

// IsBuiltinAgentID checks if the given ID is a built-in agent ID
func IsBuiltinAgentID(id string) bool {
	_, exists := BuiltinAgentRegistry[id]
	return exists
}

// GetBuiltinAgent returns a built-in agent by ID, or nil if not found
func GetBuiltinAgent(id string, tenantID uint64) *CustomAgent {
	if factory, exists := BuiltinAgentRegistry[id]; exists {
		return factory(tenantID)
	}
	return nil
}

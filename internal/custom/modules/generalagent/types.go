package generalagent

import (
	"encoding/json"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	defaultSidecarURL                   = "http://weknora-custom-general-agent:8091"
	defaultDocumentProcessingSidecarURL = "http://weknora-custom-document-processing-agent:8091"
	defaultToolCallbackURL              = "http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call"
	displayTypeArtifacts                = "general_agent_artifacts"
)

type Artifact struct {
	ID          string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID    uint64         `json:"tenant_id" gorm:"index;not null"`
	UserID      string         `json:"user_id" gorm:"type:varchar(128);index;not null"`
	RunID       string         `json:"run_id" gorm:"type:varchar(80);index;not null"`
	SessionID   string         `json:"session_id" gorm:"type:varchar(36);index;not null"`
	MessageID   string         `json:"message_id" gorm:"type:varchar(36);index"`
	FileToken   string         `json:"file_token" gorm:"type:varchar(255);not null"`
	FilePath    string         `json:"-" gorm:"type:text;not null"`
	FileName    string         `json:"filename" gorm:"type:varchar(255);not null"`
	FileType    string         `json:"file_type" gorm:"type:varchar(32);not null"`
	FileSize    int64          `json:"file_size" gorm:"not null;default:0"`
	SHA256      string         `json:"sha256" gorm:"type:varchar(64);not null"`
	ContentType string         `json:"content_type" gorm:"type:varchar(128)"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Artifact) TableName() string { return "custom_general_agent_artifacts" }

func (a *Artifact) BeforeCreate(tx *gorm.DB) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	return nil
}

type ArtifactResult struct {
	ArtifactID  string `json:"artifact_id"`
	FileName    string `json:"filename"`
	FileType    string `json:"file_type"`
	FileSize    int64  `json:"file_size"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
}

type LLMConfig struct {
	ModelName    string `json:"model_name"`
	BaseURL      string `json:"base_url"`
	APIKey       string `json:"api_key,omitempty"`
	Provider     string `json:"provider,omitempty"`
	AuthType     string `json:"auth_type,omitempty"`
	APIKeyHelper string `json:"api_key_helper,omitempty"`
}

type RuntimeToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Source      string          `json:"source"`
}

type RuntimeConfigSpec struct {
	AgentID                     string   `json:"agent_id"`
	AgentType                   string   `json:"agent_type"`
	MaxIterations               int      `json:"max_iterations"`
	Temperature                 float64  `json:"temperature"`
	Thinking                    *bool    `json:"thinking,omitempty"`
	AllowedTools                []string `json:"allowed_tools"`
	KnowledgeBases              []string `json:"knowledge_bases"`
	KnowledgeIDs                []string `json:"knowledge_ids"`
	DBDataSources               []string `json:"db_data_sources"`
	WebSearchEnabled            bool     `json:"web_search_enabled"`
	WebSearchProviderID         string   `json:"web_search_provider_id,omitempty"`
	WebSearchMaxResults         int      `json:"web_search_max_results"`
	ClaudeSDKWebSearchEnabled   bool     `json:"claude_sdk_web_search_enabled"`
	WebFetchEnabled             bool     `json:"web_fetch_enabled"`
	WebFetchTopN                int      `json:"web_fetch_top_n"`
	MultiTurnEnabled            bool     `json:"multi_turn_enabled"`
	HistoryTurns                int      `json:"history_turns"`
	MCPSelectionMode            string   `json:"mcp_selection_mode"`
	MCPServices                 []string `json:"mcp_services"`
	SkillsEnabled               bool     `json:"skills_enabled"`
	AllowedSkills               []string `json:"allowed_skills"`
	ProfessionalSkillsEnabled   bool     `json:"professional_skills_enabled"`
	AllowedProfessionalSkills   []string `json:"allowed_professional_skills"`
	RetrieveKBOnlyWhenMentioned bool     `json:"retrieve_kb_only_when_mentioned"`
	RetainRetrievalHistory      bool     `json:"retain_retrieval_history"`
	LLMCallTimeout              int      `json:"llm_call_timeout"`
	EmbeddingTopK               int      `json:"embedding_top_k"`
	KeywordThreshold            float64  `json:"keyword_threshold"`
	VectorThreshold             float64  `json:"vector_threshold"`
	RerankTopK                  int      `json:"rerank_top_k"`
	RerankThreshold             float64  `json:"rerank_threshold"`
	FAQPriorityEnabled          bool     `json:"faq_priority_enabled"`
	FAQDirectAnswerThreshold    float64  `json:"faq_direct_answer_threshold"`
	FAQScoreBoost               float64  `json:"faq_score_boost"`
}

type ProfessionalSkillFileSpec struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
}

type ProfessionalSkillSpec struct {
	Name        string                      `json:"name"`
	DisplayName string                      `json:"display_name,omitempty"`
	Description string                      `json:"description"`
	Files       []ProfessionalSkillFileSpec `json:"files"`
}

type AttachmentSpec struct {
	FileName    string `json:"file_name"`
	FileType    string `json:"file_type"`
	FileSize    int64  `json:"file_size"`
	Content     string `json:"content,omitempty"`
	IsTruncated bool   `json:"is_truncated,omitempty"`
}

type OriginalInputFileSpec struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	Role            string `json:"role"`
	FileName        string `json:"file_name"`
	FileType        string `json:"file_type"`
	FileSize        int64  `json:"file_size"`
	SHA256          string `json:"sha256"`
	DownloadURL     string `json:"download_url"`
	StorageURL      string `json:"storage_url,omitempty"`
	KnowledgeID     string `json:"knowledge_id,omitempty"`
	KnowledgeBaseID string `json:"knowledge_base_id,omitempty"`
}

type DocumentTemplateContextSpec struct {
	Files []DocumentTemplateFileSpec `json:"files,omitempty"`
}

type DocumentTemplateFileSpec struct {
	Role          string `json:"role"`
	Format        string `json:"format"`
	Variable      string `json:"variable"`
	Source        string `json:"source,omitempty"`
	BuiltinID     string `json:"builtin_id,omitempty"`
	FileName      string `json:"file_name"`
	FileType      string `json:"file_type"`
	FileSize      int64  `json:"file_size,omitempty"`
	ContentBase64 string `json:"content_base64"`
}

type ImageSpec struct {
	URL     string `json:"url"`
	Caption string `json:"caption,omitempty"`
}

type ChatHistoryMessage struct {
	Role           string                `json:"role"`
	Content        string                `json:"content"`
	MentionedItems []types.MentionedItem `json:"mentioned_items,omitempty"`
	Images         []ImageSpec           `json:"images,omitempty"`
	Attachments    []AttachmentSpec      `json:"attachments,omitempty"`
}

type ChatPayload struct {
	RunID                   string                      `json:"run_id"`
	TenantID                uint64                      `json:"tenant_id"`
	UserID                  string                      `json:"user_id"`
	SessionID               string                      `json:"session_id"`
	RequestID               string                      `json:"request_id"`
	AssistantMessageID      string                      `json:"assistant_message_id"`
	Query                   string                      `json:"query"`
	SystemPrompt            string                      `json:"system_prompt"`
	History                 []ChatHistoryMessage        `json:"history,omitempty"`
	ImageURLs               []string                    `json:"image_urls,omitempty"`
	ImageDescription        string                      `json:"image_description,omitempty"`
	QuotedContext           string                      `json:"quoted_context,omitempty"`
	SelectedSkillContext    string                      `json:"selected_skill_context,omitempty"`
	Attachments             []AttachmentSpec            `json:"attachments,omitempty"`
	OriginalInputFiles      []OriginalInputFileSpec     `json:"original_input_files,omitempty"`
	DocumentTemplateContext DocumentTemplateContextSpec `json:"document_template_context,omitempty"`
	VisibleContext          map[string]any              `json:"visible_context,omitempty"`
	ProfessionalSkills      []ProfessionalSkillSpec     `json:"professional_skills,omitempty"`
	Tools                   []RuntimeToolSpec           `json:"tools"`
	RuntimeConfig           RuntimeConfigSpec           `json:"runtime_config"`
	LLM                     *LLMConfig                  `json:"llm"`
	ToolCallbackURL         string                      `json:"tool_callback_url"`
	ToolCallbackAPIKey      string                      `json:"tool_callback_api_key,omitempty"`
	EnableArtifacts         bool                        `json:"enable_artifacts"`
}

type StreamEvent struct {
	ID        string          `json:"id,omitempty"`
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	Message   string          `json:"message,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Done      bool            `json:"done,omitempty"`
	Iteration int             `json:"iteration,omitempty"`
}

type ChatResult struct {
	RunID                 string            `json:"run_id"`
	Answer                string            `json:"answer"`
	Artifacts             []SidecarArtifact `json:"artifacts,omitempty"`
	ArtifactNotice        string            `json:"artifact_notice,omitempty"`
	ArtifactOriginalCount int               `json:"artifact_original_count,omitempty"`
	ArtifactReturnedCount int               `json:"artifact_returned_count,omitempty"`
	ArtifactDroppedCount  int               `json:"artifact_dropped_count,omitempty"`
	ArtifactReturnedSize  int64             `json:"artifact_returned_size,omitempty"`
	ArtifactLimitBytes    int64             `json:"artifact_limit_bytes,omitempty"`
}

type SidecarArtifact struct {
	FileToken   string `json:"file_token"`
	FileName    string `json:"filename"`
	FileType    string `json:"file_type"`
	FileSize    int64  `json:"file_size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
}

type ToolCallRequest struct {
	RunID      string          `json:"run_id"`
	ToolName   string          `json:"tool_name"`
	Arguments  json.RawMessage `json:"arguments"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type ToolCallResponse struct {
	Success bool                   `json:"success"`
	Output  string                 `json:"output"`
	Error   string                 `json:"error,omitempty"`
	Data    map[string]interface{} `json:"data,omitempty"`
	Images  []string               `json:"images,omitempty"`
}

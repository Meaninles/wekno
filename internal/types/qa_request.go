package types

// QARequest consolidates all parameters for KnowledgeQA and AgentQA service calls,
// replacing the previous 14-parameter method signatures.
// EventBus is passed separately to avoid circular dependency with the event package.
type QARequest struct {
	Session            *Session            // The conversation session
	RequestID          string              // Request ID used by SSE/tool approval correlation
	Query              string              // User query text
	AssistantMessageID string              // Pre-created assistant message ID
	SummaryModelID     string              // Optional model override; empty = use agent/KB default
	CustomAgent        *CustomAgent        // Optional custom agent for config override
	KnowledgeBaseIDs   []string            // Knowledge base IDs to search (from request + @mentions)
	KnowledgeIDs       []string            // Specific knowledge (file) IDs to search
	TagScopes          []TagScope          // Tag-constrained KB scopes from @mentions
	MCPServiceIDs      []string            // Per-request MCP service IDs from @mentions
	SkillNames         []string            // Per-request preloaded skill names from @mentions
	ImageURLs          []string            // Image URLs for multimodal input
	ImageDescription   string              // VLM-generated image description (fallback for non-vision models)
	UserMessageID      string              // Created user message ID
	WebSearchEnabled   bool                // Whether web search is enabled for this request
	EnableMemory       bool                // Whether memory feature is enabled
	QuotedContext      string              // Quoted message content from IM quote-reply (appended at LLM prompt stage, not used for retrieval)
	Attachments        MessageAttachments  // File attachments (processed and ready for prompt injection)
	OriginalInputFiles []OriginalInputFile // Runtime-only original file descriptors for Claude SDK agents
}

const (
	OriginalInputSourceChatUpload        = "weknora_chat_upload_original"
	OriginalInputSourceChatImage         = "weknora_chat_image_original"
	OriginalInputSourceSelectedKnowledge = "weknora_selected_knowledge_original"

	OriginalInputRoleUserUploadedOriginal      = "user_uploaded_original_file"
	OriginalInputRoleSelectedKnowledgeOriginal = "selected_knowledge_original_file"
)

// OriginalInputFile describes a byte-verified original file copy that a Claude
// SDK sidecar must download into its run directory before the SDK starts.
// DownloadURL is runtime-only and must never be shown to the model prompt.
type OriginalInputFile struct {
	ID              string
	Source          string
	Role            string
	FileName        string
	FileType        string
	FileSize        int64
	SHA256          string
	DownloadURL     string
	StorageURL      string
	KnowledgeID     string
	KnowledgeBaseID string
}

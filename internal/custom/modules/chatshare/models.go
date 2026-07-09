package chatshare

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

const ShareStatusActive = "active"

type Link struct {
	ID              string         `json:"id" gorm:"primaryKey;type:varchar(36)"`
	TokenHash       string         `json:"-" gorm:"type:varchar(64);uniqueIndex;not null"`
	SourceTenantID  uint64         `json:"source_tenant_id" gorm:"not null;index"`
	SessionID       string         `json:"session_id" gorm:"type:varchar(36);not null;index"`
	SourceUserID    string         `json:"source_user_id" gorm:"type:varchar(36);index"`
	CreatedByUserID string         `json:"created_by_user_id" gorm:"type:varchar(36);index"`
	Title           string         `json:"title" gorm:"type:varchar(255)"`
	Status          string         `json:"status" gorm:"type:varchar(32);not null;default:'active';index"`
	ViewCount       int64          `json:"view_count" gorm:"default:0"`
	LastViewedAt    *time.Time     `json:"last_viewed_at,omitempty"`
	RevokedAt       *time.Time     `json:"revoked_at,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Link) TableName() string {
	return "custom_chatshare_links"
}

func (l *Link) BeforeCreate(_ *gorm.DB) error {
	if l.ID == "" {
		l.ID = uuid.NewString()
	}
	if l.Status == "" {
		l.Status = ShareStatusActive
	}
	return nil
}

type MessageSnapshot struct {
	ID                  string                   `json:"id" gorm:"primaryKey;type:varchar(36)"`
	ShareID             string                   `json:"share_id" gorm:"type:varchar(36);not null;index"`
	Seq                 int                      `json:"seq" gorm:"not null;index"`
	OriginalMessageID   string                   `json:"original_message_id" gorm:"type:varchar(36);index"`
	SessionID           string                   `json:"session_id" gorm:"type:varchar(36);not null;index"`
	RequestID           string                   `json:"request_id" gorm:"type:varchar(64)"`
	Content             string                   `json:"content" gorm:"type:text"`
	Role                string                   `json:"role" gorm:"type:varchar(32);not null"`
	KnowledgeReferences types.References         `json:"knowledge_references,omitempty" gorm:"type:jsonb;column:knowledge_references"`
	MentionedItems      types.MentionedItems     `json:"mentioned_items,omitempty" gorm:"type:jsonb;column:mentioned_items"`
	Images              types.MessageImages      `json:"images,omitempty" gorm:"type:jsonb;column:images"`
	Attachments         types.MessageAttachments `json:"attachments,omitempty" gorm:"type:jsonb;column:attachments"`
	ToolResults         ToolResultSnapshots      `json:"tool_results,omitempty" gorm:"type:jsonb;column:tool_results"`
	Artifacts           []ArtifactSnapshot       `json:"artifacts,omitempty" gorm:"-"`
	IsCompleted         bool                     `json:"is_completed"`
	IsFallback          bool                     `json:"is_fallback,omitempty"`
	Channel             string                   `json:"channel,omitempty" gorm:"type:varchar(50)"`
	CreatedAt           time.Time                `json:"created_at"`
	UpdatedAt           time.Time                `json:"updated_at"`
}

func (MessageSnapshot) TableName() string {
	return "custom_chatshare_messages"
}

func (m *MessageSnapshot) BeforeCreate(_ *gorm.DB) error {
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	if m.KnowledgeReferences == nil {
		m.KnowledgeReferences = make(types.References, 0)
	}
	if m.MentionedItems == nil {
		m.MentionedItems = make(types.MentionedItems, 0)
	}
	if m.Images == nil {
		m.Images = make(types.MessageImages, 0)
	}
	if m.Attachments == nil {
		m.Attachments = make(types.MessageAttachments, 0)
	}
	if m.ToolResults == nil {
		m.ToolResults = make(ToolResultSnapshots, 0)
	}
	return nil
}

type ToolResultSnapshots []ToolResultSnapshot

func (t ToolResultSnapshots) Value() (driver.Value, error) {
	if t == nil {
		return json.Marshal([]ToolResultSnapshot{})
	}
	return json.Marshal(t)
}

func (t *ToolResultSnapshots) Scan(value interface{}) error {
	if value == nil {
		*t = make(ToolResultSnapshots, 0)
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("failed to scan ToolResultSnapshots: unsupported type %T", value)
	}
	if len(b) == 0 {
		*t = make(ToolResultSnapshots, 0)
		return nil
	}
	return json.Unmarshal(b, t)
}

type ToolResultSnapshot struct {
	DisplayType string                 `json:"display_type"`
	ToolData    map[string]interface{} `json:"tool_data,omitempty"`
	Output      string                 `json:"output,omitempty"`
	Arguments   map[string]interface{} `json:"arguments,omitempty"`
}

type ArtifactSnapshot struct {
	ArtifactID  string `json:"artifact_id"`
	Filename    string `json:"filename"`
	FileType    string `json:"file_type"`
	FileSize    int64  `json:"file_size"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
}

type LinkDTO struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Token     string    `json:"token,omitempty"`
	URL       string    `json:"url,omitempty"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

type ViewDTO struct {
	ID        string            `json:"id"`
	SessionID string            `json:"session_id"`
	Title     string            `json:"title"`
	CreatedAt time.Time         `json:"created_at"`
	Messages  []MessageSnapshot `json:"messages"`
}

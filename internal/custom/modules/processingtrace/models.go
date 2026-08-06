package processingtrace

import "time"

type Span struct {
	KnowledgeID            string     `json:"knowledge_id" gorm:"type:varchar(36);primaryKey"`
	Attempt                int        `json:"attempt" gorm:"primaryKey"`
	LogicalKey             string     `json:"logical_key" gorm:"type:varchar(190);primaryKey"`
	SpanID                 string     `json:"span_id" gorm:"type:varchar(64);not null;uniqueIndex"`
	ParentLogicalKey       string     `json:"parent_logical_key" gorm:"type:varchar(190);not null;default:''"`
	Name                   string     `json:"name" gorm:"type:varchar(160);not null"`
	Kind                   string     `json:"kind" gorm:"type:varchar(32);not null"`
	Status                 string     `json:"status" gorm:"type:varchar(32);not null;index"`
	RealAttemptCount       int        `json:"real_attempt_count" gorm:"not null;default:0"`
	InputSummary           string     `json:"input_summary" gorm:"type:text;not null;default:''"`
	OutputSummary          string     `json:"output_summary" gorm:"type:text;not null;default:''"`
	MetadataSummary        string     `json:"metadata_summary" gorm:"type:text;not null;default:''"`
	LastErrorCode          string     `json:"last_error_code" gorm:"type:varchar(64);not null;default:''"`
	LastErrorMessage       string     `json:"last_error_message" gorm:"type:text;not null;default:''"`
	LastErrorDetail        string     `json:"-" gorm:"type:text;not null;default:''"`
	StartedAt              time.Time  `json:"started_at" gorm:"not null"`
	LastBusinessProgressAt *time.Time `json:"last_business_progress_at,omitempty"`
	FinishedAt             *time.Time `json:"finished_at,omitempty"`
	DurationMS             int64      `json:"duration_ms" gorm:"not null;default:0"`
	CreatedAt              time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt              time.Time  `json:"updated_at" gorm:"not null;index"`
}

func (Span) TableName() string { return "custom_processing_spans_v2" }

// knowledgeLifecycleSchema is deliberately kept in the custom module so the
// native Knowledge model only carries the fields used by business code. The
// dedicated migration role adds these columns without letting serving
// replicas perform DDL.
type knowledgeLifecycleSchema struct {
	CoreStatus            string     `gorm:"column:core_status;type:varchar(32);not null;default:'pending'"`
	CoreCompletedAt       *time.Time `gorm:"column:core_completed_at"`
	EnrichmentCompletedAt *time.Time `gorm:"column:enrichment_completed_at"`
	EnrichmentError       string     `gorm:"column:enrichment_error_summary;type:text;not null;default:''"`
}

func (knowledgeLifecycleSchema) TableName() string { return "knowledges" }

type Upsert struct {
	KnowledgeID            string
	Attempt                int
	LogicalKey             string
	ParentLogicalKey       string
	Name                   string
	Kind                   string
	Status                 string
	InputSummary           string
	OutputSummary          string
	MetadataSummary        string
	LastErrorCode          string
	LastErrorMessage       string
	LastErrorDetail        string
	StartedAt              time.Time
	LastBusinessProgressAt *time.Time
	FinishedAt             *time.Time
	IncrementRealAttempt   bool
	DecrementRealAttempt   bool
}

type Cursor struct {
	LogicalKey string
}

type Page struct {
	Items      []Span  `json:"items"`
	NextCursor *Cursor `json:"next_cursor,omitempty"`
}

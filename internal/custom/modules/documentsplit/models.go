package documentsplit

import (
	"encoding/json"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

type PlanState string

const (
	PlanPreparing  PlanState = "preparing"
	PlanQueued     PlanState = "queued"
	PlanParsing    PlanState = "parsing"
	PlanFinalizing PlanState = "finalizing"
	PlanCompleted  PlanState = "completed"
	PlanFailed     PlanState = "failed"
	PlanCancelled  PlanState = "cancelled"
	PlanSuperseded PlanState = "superseded"
)

type PartState string

const (
	PartPreparing PartState = "preparing"
	PartQueued    PartState = "queued"
	PartLeased    PartState = "leased"
	PartCompleted PartState = "completed"
	PartFailed    PartState = "failed"
	PartCancelled PartState = "cancelled"
)

// Plan is the durable physical representation of one logical document
// generation. Redis is only a wake-up mechanism; this row and its Part rows
// are the source of truth for recovery.
type Plan struct {
	ID                   string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID             uint64    `json:"tenant_id" gorm:"not null;index;uniqueIndex:idx_custom_document_split_generation"`
	KnowledgeBaseID      string    `json:"knowledge_base_id" gorm:"type:varchar(36);not null;index"`
	KnowledgeID          string    `json:"knowledge_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_document_split_generation"`
	ProcessingGeneration string    `json:"processing_generation" gorm:"type:varchar(64);not null;uniqueIndex:idx_custom_document_split_generation"`
	ProcessingOwner      string    `json:"-" gorm:"type:varchar(160);not null"`
	SourcePath           string    `json:"-" gorm:"type:text;not null"`
	SourceName           string    `json:"source_name" gorm:"type:text;not null"`
	SourceType           string    `json:"source_type" gorm:"type:varchar(32);not null"`
	SourceSize           int64     `json:"source_size" gorm:"not null"`
	SourceSHA256         string    `json:"source_sha256" gorm:"type:varchar(64);not null"`
	PlannerVersion       string    `json:"planner_version" gorm:"type:varchar(64);not null;default:''"`
	RulesHash            string    `json:"rules_hash" gorm:"type:varchar(64);not null;default:''"`
	Strategy             string    `json:"strategy" gorm:"type:varchar(64);not null;default:''"`
	State                PlanState `json:"state" gorm:"type:varchar(24);not null;index"`
	PartCount            int       `json:"part_count" gorm:"not null;default:0"`
	CompletedParts       int       `json:"completed_parts" gorm:"not null;default:0"`
	FailedParts          int       `json:"failed_parts" gorm:"not null;default:0"`
	TotalPartBytes       int64     `json:"total_part_bytes" gorm:"not null;default:0"`
	TargetRatio          float64   `json:"target_ratio" gorm:"not null;default:0.75"`
	Attempt              int       `json:"attempt" gorm:"not null;default:1"`
	LastError            string    `json:"last_error,omitempty" gorm:"type:text"`
	LastProgressAt       time.Time `json:"last_progress_at" gorm:"not null;index"`
	FinalizerTaskID      string    `json:"finalizer_task_id,omitempty" gorm:"type:varchar(180);not null;default:''"`
	Version              int64     `json:"version" gorm:"not null;default:1"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func (Plan) TableName() string { return "custom_document_split_plans" }

type Part struct {
	ID                   string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	PlanID               string     `json:"plan_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_document_split_part"`
	TenantID             uint64     `json:"tenant_id" gorm:"not null;index"`
	KnowledgeBaseID      string     `json:"knowledge_base_id" gorm:"type:varchar(36);not null;index"`
	KnowledgeID          string     `json:"knowledge_id" gorm:"type:varchar(36);not null;index"`
	ProcessingGeneration string     `json:"processing_generation" gorm:"type:varchar(64);not null;index"`
	PartIndex            int        `json:"part_index" gorm:"not null;uniqueIndex:idx_custom_document_split_part"`
	FileName             string     `json:"file_name" gorm:"type:text;not null"`
	FileType             string     `json:"file_type" gorm:"type:varchar(32);not null"`
	InputPath            string     `json:"-" gorm:"type:text;not null"`
	InputSize            int64      `json:"input_size" gorm:"not null"`
	InputSHA256          string     `json:"input_sha256" gorm:"type:varchar(64);not null"`
	Locator              types.JSON `json:"locator" gorm:"type:jsonb;not null"`
	Metrics              types.JSON `json:"metrics" gorm:"type:jsonb;not null"`
	State                PartState  `json:"state" gorm:"type:varchar(24);not null;index"`
	// Attempt counts execution leases for diagnostics only. A worker/process
	// restart may increase it without consuming the business retry budget.
	Attempt int `json:"execution_attempts" gorm:"column:attempt;not null;default:0"`
	// FailureAttempts counts only completed, classified business failures.
	// Lease expiry, process replacement and dependency deferral never change it.
	FailureAttempts int `json:"failure_attempts" gorm:"not null;default:0"`
	// BackpressureEvents remembers provider/infrastructure throttling so the
	// document resumes with one probe instead of reopening a burst window.
	BackpressureEvents int `json:"backpressure_events" gorm:"not null;default:0"`
	// DispatchEpoch fences disposable Redis/Asynq wake-ups independently of
	// execution leases. A missing or archived wake-up can be replaced without
	// pretending that the document ran or failed.
	DispatchEpoch      int64      `json:"dispatch_epoch" gorm:"not null;default:0"`
	DispatchLeaseUntil *time.Time `json:"dispatch_lease_until,omitempty" gorm:"index"`
	LeaseEpoch         int64      `json:"lease_epoch" gorm:"not null;default:0"`
	LeaseOwner         string     `json:"lease_owner,omitempty" gorm:"type:varchar(160);not null;default:'';index"`
	LeaseInstanceID    string     `json:"lease_instance_id,omitempty" gorm:"type:varchar(255);not null;default:'';index"`
	LeaseBootID        string     `json:"lease_boot_id,omitempty" gorm:"type:varchar(64);not null;default:'';index"`
	LeaseUntil         *time.Time `json:"lease_until,omitempty" gorm:"index"`
	OutputPath         string     `json:"-" gorm:"type:text;not null;default:''"`
	OutputSize         int64      `json:"output_size" gorm:"not null;default:0"`
	OutputSHA256       string     `json:"output_sha256,omitempty" gorm:"type:varchar(64);not null;default:''"`
	MarkdownChars      int64      `json:"markdown_chars" gorm:"not null;default:0"`
	DraftChunks        int        `json:"draft_chunks" gorm:"not null;default:0"`
	StorageBytes       int64      `json:"storage_bytes" gorm:"not null;default:0"`
	FirstChunkID       string     `json:"first_chunk_id,omitempty" gorm:"type:varchar(36);not null;default:''"`
	LastChunkID        string     `json:"last_chunk_id,omitempty" gorm:"type:varchar(36);not null;default:''"`
	ImageMappings      types.JSON `json:"image_mappings,omitempty" gorm:"type:jsonb"`
	LastError          string     `json:"last_error,omitempty" gorm:"type:text"`
	LastProgressAt     time.Time  `json:"last_progress_at" gorm:"not null;index"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	Version            int64      `json:"version" gorm:"not null;default:1"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func (Part) TableName() string { return "custom_document_split_parts" }

type Manifest struct {
	SchemaVersion  int            `json:"schema_version"`
	PlannerVersion string         `json:"planner_version"`
	CreatedUnixMS  int64          `json:"created_unix_ms"`
	Strategy       string         `json:"strategy"`
	TargetRatio    float64        `json:"target_ratio"`
	PartCount      int            `json:"part_count"`
	TotalPartBytes int64          `json:"total_part_bytes"`
	Source         ManifestSource `json:"source"`
	Parts          []ManifestPart `json:"parts"`
}

type ManifestSource struct {
	FileName  string `json:"file_name"`
	FileType  string `json:"file_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type ManifestPart struct {
	Index     int             `json:"index"`
	FileName  string          `json:"file_name"`
	FileType  string          `json:"file_type"`
	SizeBytes int64           `json:"size_bytes"`
	SHA256    string          `json:"sha256"`
	Locator   json.RawMessage `json:"locator"`
	Metrics   json.RawMessage `json:"metrics"`
}

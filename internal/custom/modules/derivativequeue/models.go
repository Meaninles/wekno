// Package derivativequeue owns the PostgreSQL-authoritative lifecycle for
// summary, question, graph, and derivative-finalization work. Redis/Asynq is
// deliberately only a wake-up transport; every transition is fenced here.
package derivativequeue

import (
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	StateQueued            = "queued"
	StateLeased            = "leased"
	StateAdmitted          = "admitted"
	StateProviderRunning   = "provider_running"
	StateProviderSucceeded = "provider_succeeded"
	StateProviderUnknown   = "provider_unknown"
	StateRetryWait         = "retry_wait"
	StateMaterializing     = "materializing"
	StateMaterializeWait   = "materialize_wait"
	StateMaterialized      = "materialized"
	StateFinalizing        = "finalizing"
	StateFinalizeWait      = "finalize_wait"
	StateCompleted         = "completed"
	StateCancelled         = "cancelled"
	StateFailed            = "failed"
)

const (
	WorkSummary    = "summary"
	WorkQuestion   = "question_batch"
	WorkGraph      = "graph_batch"
	WorkDataTable  = "datatable_metadata"
	WorkFinalizer  = "finalizer"
	DefaultLane    = "normal"
	MaxPayloadSize = 256 * 1024

	ProviderCallCheckpointed    = "checkpointed"
	ProviderCallAccepted        = "accepted"
	ProviderCallInvalidContract = "invalid_contract"
)

type WorkItem struct {
	ID                   string     `json:"id" gorm:"type:uuid;primaryKey"`
	TenantID             uint64     `json:"tenant_id" gorm:"not null;index:idx_derivative_scope,priority:1;uniqueIndex:uq_derivative_item,priority:1"`
	KnowledgeBaseID      string     `json:"knowledge_base_id" gorm:"type:varchar(36);not null;index:idx_derivative_scope,priority:2"`
	KnowledgeID          string     `json:"knowledge_id" gorm:"type:varchar(36);not null;index:idx_derivative_generation,priority:1;uniqueIndex:uq_derivative_item,priority:2"`
	ProcessingGeneration string     `json:"processing_generation" gorm:"type:varchar(64);not null;index:idx_derivative_generation,priority:2;uniqueIndex:uq_derivative_item,priority:3"`
	ProcessingAttempt    int        `json:"processing_attempt" gorm:"not null;default:1"`
	ItemID               string     `json:"item_id" gorm:"type:varchar(160);not null;uniqueIndex:uq_derivative_item,priority:4"`
	WorkKind             string     `json:"work_kind" gorm:"type:varchar(32);not null;index:idx_derivative_due,priority:6"`
	Payload              types.JSON `json:"payload" gorm:"type:jsonb;not null"`
	PayloadHash          string     `json:"payload_hash" gorm:"type:varchar(64);not null"`

	ModelID        string `json:"model_id" gorm:"type:varchar(64);not null;default:''"`
	ModelTenantID  uint64 `json:"model_tenant_id" gorm:"not null;default:0"`
	ResourcePoolID string `json:"resource_pool_id" gorm:"type:varchar(64);not null;default:'';index:idx_derivative_pool_due,priority:1"`
	QuotaPoolID    string `json:"quota_pool_id" gorm:"type:varchar(64);not null;default:''"`
	GatewayPoolID  string `json:"gateway_pool_id" gorm:"type:varchar(64);not null;default:''"`
	PolicyVersion  uint64 `json:"policy_version" gorm:"not null;default:0"`

	State              string     `json:"state" gorm:"type:varchar(32);not null;index:idx_derivative_due,priority:1;index:idx_derivative_pool_due,priority:2;index:idx_derivative_lease,priority:1"`
	Priority           int        `json:"priority" gorm:"not null;default:0;index:idx_derivative_due,priority:3,sort:desc"`
	QueueLane          string     `json:"queue_lane" gorm:"type:varchar(24);not null;default:'normal'"`
	DispatchEpoch      uint64     `json:"dispatch_epoch" gorm:"not null;default:0"`
	DispatchTaskID     string     `json:"dispatch_task_id" gorm:"type:varchar(190);not null;default:''"`
	DispatchLeaseUntil *time.Time `json:"dispatch_lease_until,omitempty" gorm:"index:idx_derivative_dispatch_lease"`

	OwnerInstanceID string     `json:"owner_instance_id" gorm:"type:varchar(160);not null;default:''"`
	LeaseToken      string     `json:"-" gorm:"type:varchar(64);not null;default:''"`
	LeaseUntil      *time.Time `json:"lease_until,omitempty" gorm:"index:idx_derivative_lease,priority:2"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`

	ProviderRequestKey  string `json:"provider_request_key" gorm:"type:varchar(160);not null;default:'';index"`
	ProviderAttempts    int    `json:"provider_attempts" gorm:"not null;default:0"`
	MaterializeAttempts int    `json:"materialize_attempts" gorm:"not null;default:0"`
	FinalizeAttempts    int    `json:"finalize_attempts" gorm:"not null;default:0"`

	NextAttemptAt    time.Time `json:"next_attempt_at" gorm:"not null;index:idx_derivative_due,priority:2;index:idx_derivative_pool_due,priority:3"`
	LastErrorClass   string    `json:"last_error_class" gorm:"type:varchar(32);not null;default:''"`
	LastErrorCode    string    `json:"last_error_code" gorm:"type:varchar(64);not null;default:''"`
	LastErrorMessage string    `json:"last_error_message" gorm:"type:text;not null;default:''"`

	ResultID    *string    `json:"result_id,omitempty" gorm:"type:uuid"`
	Version     uint64     `json:"version" gorm:"not null;default:1"`
	CreatedAt   time.Time  `json:"created_at" gorm:"not null;index:idx_derivative_due,priority:4"`
	UpdatedAt   time.Time  `json:"updated_at" gorm:"not null"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (WorkItem) TableName() string { return "custom_derivative_work_items" }

type Result struct {
	ID                 string     `json:"id" gorm:"type:uuid;primaryKey"`
	WorkItemID         string     `json:"work_item_id" gorm:"type:uuid;not null;uniqueIndex"`
	ProviderRequestKey string     `json:"provider_request_key" gorm:"type:varchar(160);not null;uniqueIndex"`
	ModelID            string     `json:"model_id" gorm:"type:varchar(64);not null;default:''"`
	ResourcePoolID     string     `json:"resource_pool_id" gorm:"type:varchar(64);not null;default:''"`
	ResponseContent    string     `json:"response_content,omitempty" gorm:"type:text;not null;default:''"`
	ResponseURI        string     `json:"response_uri,omitempty" gorm:"type:text;not null;default:''"`
	ResponseSize       int64      `json:"response_size" gorm:"not null;default:0"`
	ResponseUsage      types.JSON `json:"response_usage" gorm:"type:jsonb;not null"`
	ResponseMetadata   types.JSON `json:"response_metadata" gorm:"type:jsonb;not null"`
	ContentChecksum    string     `json:"content_checksum" gorm:"type:varchar(64);not null"`
	ProviderRequestID  string     `json:"provider_request_id" gorm:"type:varchar(160);not null;default:''"`
	CreatedAt          time.Time  `json:"created_at" gorm:"not null"`
	MaterializedAt     *time.Time `json:"materialized_at,omitempty"`
	ExpiresAt          time.Time  `json:"expires_at" gorm:"not null;index"`
}

func (Result) TableName() string { return "custom_derivative_results" }

// ProviderCall is an immutable checkpoint for one actual outbound model
// request made while executing a WorkItem. The request hash is derived from
// the resolved model plus the exact messages/options. A worker persists this
// row before returning the response to business materialization code; a
// redelivery replays it and therefore never pays for the same provider call
// twice.
type ProviderCall struct {
	ID                   string     `json:"id" gorm:"type:uuid;primaryKey"`
	WorkItemID           string     `json:"work_item_id" gorm:"type:uuid;not null;uniqueIndex:uq_custom_derivative_provider_call,priority:1;index"`
	RequestHash          string     `json:"request_hash" gorm:"type:varchar(64);not null;uniqueIndex:uq_custom_derivative_provider_call,priority:2"`
	Attempt              int        `json:"attempt" gorm:"not null;default:1;uniqueIndex:uq_custom_derivative_provider_call,priority:3"`
	ProviderRequestKey   string     `json:"provider_request_key" gorm:"type:varchar(190);not null;uniqueIndex"`
	ProviderRequestID    string     `json:"provider_request_id" gorm:"type:varchar(160);not null;default:''"`
	ProcessingGeneration string     `json:"processing_generation" gorm:"type:varchar(64);not null;default:'';index"`
	ModelID              string     `json:"model_id" gorm:"type:varchar(64);not null;default:''"`
	Response             types.JSON `json:"response" gorm:"type:jsonb;not null"`
	ResponseSize         int64      `json:"response_size" gorm:"not null;default:0"`
	ContentChecksum      string     `json:"content_checksum" gorm:"type:varchar(64);not null"`
	Disposition          string     `json:"disposition" gorm:"type:varchar(32);not null;default:'checkpointed';index"`
	ValidationError      string     `json:"validation_error" gorm:"type:text;not null;default:''"`
	CreatedAt            time.Time  `json:"created_at" gorm:"not null"`
	ValidatedAt          *time.Time `json:"validated_at,omitempty"`
}

func (ProviderCall) TableName() string { return "custom_derivative_provider_calls" }

type WakePayload struct {
	WorkItemID    string `json:"work_item_id"`
	DispatchEpoch uint64 `json:"dispatch_epoch"`
}

type PlanItem struct {
	TenantID             uint64
	KnowledgeBaseID      string
	KnowledgeID          string
	ProcessingGeneration string
	ProcessingAttempt    int
	ItemID               string
	WorkKind             string
	Payload              types.JSON
	ModelID              string
	ModelTenantID        uint64
	ResourcePoolID       string
	QuotaPoolID          string
	GatewayPoolID        string
	PolicyVersion        uint64
	Priority             int
	QueueLane            string
}

type ProviderResult struct {
	Content           string
	URI               string
	Size              int64
	Usage             types.JSON
	Metadata          types.JSON
	ProviderRequestID string
}

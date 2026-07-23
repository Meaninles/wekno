package documentqueue

import "time"

// WorkflowState is the durable state of one document processing generation.
// Redis/Asynq is only a delivery mechanism; this row is the source of truth
// used to recover work after either Redis or an application instance restarts.
type WorkflowState string

const (
	StatePreparing  WorkflowState = "preparing"
	StateQueued     WorkflowState = "queued"
	StateLeased     WorkflowState = "leased"
	StateCompleted  WorkflowState = "completed"
	StateFailed     WorkflowState = "failed"
	StateCancelled  WorkflowState = "cancelled"
	StateSuperseded WorkflowState = "superseded"
)

const StagePreparing = "preparing"

// ReparsePendingTransition is the business state committed together with an
// exact prepared workflow binding. Keeping this shape deliberately narrow
// prevents producer code from smuggling unrelated knowledge mutations through
// the document-queue transaction.
type ReparsePendingTransition struct {
	EmbeddingModelID string
	ErrorMessage     string
	UpdatedAt        time.Time
}

const (
	InstanceStarting = "starting"
	InstanceReady    = "ready"
	InstanceDegraded = "degraded"
	InstanceDraining = "draining"
	InstanceStopped  = "stopped"
)

// Workflow stores a PostgreSQL outbox entry and the current execution lease.
// (knowledge_id, processing_generation) is immutable and unique, which makes
// repeated producers and delayed Asynq deliveries converge on one workflow.
type Workflow struct {
	ID                   string        `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID             uint64        `json:"tenant_id" gorm:"not null;index;uniqueIndex:idx_custom_document_workflow_generation"`
	KnowledgeBaseID      string        `json:"knowledge_base_id" gorm:"type:varchar(36);not null;index"`
	KnowledgeID          string        `json:"knowledge_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_document_workflow_generation"`
	ProcessingGeneration string        `json:"processing_generation" gorm:"type:varchar(64);not null;uniqueIndex:idx_custom_document_workflow_generation"`
	TaskType             string        `json:"task_type" gorm:"type:varchar(64);not null"`
	Payload              []byte        `json:"-" gorm:"type:jsonb;not null"`
	PlanHash             string        `json:"plan_hash" gorm:"type:varchar(64);not null"`
	State                WorkflowState `json:"state" gorm:"type:varchar(24);not null;index"`
	Stage                string        `json:"stage" gorm:"type:varchar(32);not null;default:'queued'"`
	DispatchEpoch        int64         `json:"dispatch_epoch" gorm:"not null;default:1"`
	DispatchTaskID       string        `json:"dispatch_task_id" gorm:"type:varchar(160);not null;default:''"`
	DispatchAttempts     int           `json:"dispatch_attempts" gorm:"not null;default:0"`
	MaxRetry             int           `json:"max_retry" gorm:"not null;default:3"`
	DelegateTimeoutNanos int64         `json:"delegate_timeout_nanos" gorm:"not null;default:0"`
	WorkflowTimeoutNanos int64         `json:"workflow_timeout_nanos" gorm:"not null;default:0"`
	DeadlineAt           *time.Time    `json:"deadline_at,omitempty"`
	RetentionNanos       int64         `json:"retention_nanos" gorm:"not null;default:0"`
	OwnerInstanceID      string        `json:"owner_instance_id" gorm:"type:varchar(160);not null;default:'';index"`
	OwnerBootID          string        `json:"owner_boot_id" gorm:"type:varchar(36);not null;default:''"`
	LeaseUntil           *time.Time    `json:"lease_until,omitempty" gorm:"index"`
	EnqueuedAt           time.Time     `json:"enqueued_at" gorm:"not null;index"`
	StartedAt            *time.Time    `json:"started_at,omitempty"`
	LastDispatchedAt     *time.Time    `json:"last_dispatched_at,omitempty"`
	LastHeartbeatAt      *time.Time    `json:"last_heartbeat_at,omitempty"`
	LastProgressAt       *time.Time    `json:"last_progress_at,omitempty" gorm:"index"`
	CompletedAt          *time.Time    `json:"completed_at,omitempty"`
	LastError            string        `json:"last_error,omitempty" gorm:"type:text"`
	Version              int64         `json:"version" gorm:"not null;default:1"`
	CreatedAt            time.Time     `json:"created_at"`
	UpdatedAt            time.Time     `json:"updated_at"`
}

func (Workflow) TableName() string { return "custom_document_queue_workflows" }

// Instance records a stable parser identity and one process incarnation.
// InstanceID should be a runtime-enforced identity (the bundled Helm chart
// uses namespace/Pod UID), while BootID changes on every process start. A new
// BootID may immediately adopt prior leases only when the runtime guarantees
// that two live containers cannot hold the same InstanceID.
type Instance struct {
	InstanceID      string     `json:"instance_id" gorm:"type:varchar(160);primaryKey"`
	BootID          string     `json:"boot_id" gorm:"type:varchar(36);not null;index"`
	State           string     `json:"state" gorm:"type:varchar(24);not null;index"`
	Capacity        int        `json:"capacity" gorm:"not null"`
	StartedAt       time.Time  `json:"started_at" gorm:"not null"`
	LastHeartbeatAt time.Time  `json:"last_heartbeat_at" gorm:"not null;index"`
	StoppedAt       *time.Time `json:"stopped_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (Instance) TableName() string { return "custom_document_queue_instances" }

type QueueItem struct {
	Position        int64      `json:"position"`
	State           string     `json:"state"`
	Stage           string     `json:"stage,omitempty"`
	OwnerInstanceID string     `json:"owner_instance_id,omitempty"`
	OwnerBootID     string     `json:"owner_boot_id,omitempty"`
	ExecutionEpoch  int64      `json:"execution_epoch,omitempty"`
	LeaseUntil      *time.Time `json:"lease_until,omitempty"`
	LastProgressAt  *time.Time `json:"last_progress_at,omitempty"`
}

type QueueStatus struct {
	WaitingTotal  int64                `json:"waiting_total"`
	ActiveTotal   int64                `json:"active_total"`
	CapacityTotal int64                `json:"capacity_total"`
	Items         map[string]QueueItem `json:"items"`
}

type InstanceStatus struct {
	Instance
	ActiveCount     int64    `json:"active_count"`
	ActiveDocuments []string `json:"active_documents"`
	Healthy         bool     `json:"healthy"`
}

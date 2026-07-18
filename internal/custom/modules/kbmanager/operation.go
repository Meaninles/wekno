package kbmanager

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	OperationTypeAdd     = "add"
	OperationTypeReplace = "replace"
	OperationTypeDelete  = "delete"

	OperationStatePreparing = "preparing"
	OperationStateParsing   = "parsing"
	OperationStateCompleted = "completed"
	OperationStateFailed    = "failed"
	OperationStateDuplicate = "duplicate"
	OperationStateNoChange  = "no_change"
)

// Operation is both the durable workflow state and the mutation audit record.
// It contains source identifiers/hashes but never source bytes, URLs, paths, or
// credentials.
type Operation struct {
	ID              string     `json:"id" gorm:"type:varchar(36);primaryKey"`
	AgentID         string     `json:"agent_id" gorm:"type:varchar(36);index;not null"`
	AgentTenantID   uint64     `json:"agent_tenant_id" gorm:"index;not null"`
	SessionID       string     `json:"session_id" gorm:"type:varchar(36);index"`
	RunID           string     `json:"run_id" gorm:"type:varchar(80);index"`
	CallerTenantID  uint64     `json:"caller_tenant_id" gorm:"index;not null"`
	SourceTenantID  uint64     `json:"source_tenant_id" gorm:"index;not null"`
	UserID          string     `json:"user_id" gorm:"type:varchar(128);index;not null"`
	CallerRole      string     `json:"caller_role" gorm:"type:varchar(20);not null"`
	Type            string     `json:"type" gorm:"type:varchar(20);index;not null"`
	State           string     `json:"state" gorm:"type:varchar(32);index;not null"`
	KnowledgeBaseID string     `json:"knowledge_base_id" gorm:"type:varchar(36);index;not null"`
	OldKnowledgeID  string     `json:"old_knowledge_id,omitempty" gorm:"type:varchar(36);index"`
	NewKnowledgeID  string     `json:"new_knowledge_id,omitempty" gorm:"type:varchar(36);index"`
	OldFileHash     string     `json:"old_file_hash,omitempty" gorm:"type:varchar(128)"`
	SourceKind      string     `json:"source_kind,omitempty" gorm:"type:varchar(24)"`
	SourceID        string     `json:"source_id,omitempty" gorm:"type:varchar(255)"`
	SourceSHA256    string     `json:"source_sha256,omitempty" gorm:"type:varchar(64)"`
	FileName        string     `json:"file_name,omitempty" gorm:"type:varchar(255)"`
	Reason          string     `json:"reason,omitempty" gorm:"type:text"`
	ResultMessage   string     `json:"result_message,omitempty" gorm:"type:text"`
	ErrorMessage    string     `json:"error_message,omitempty" gorm:"type:text"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

func (Operation) TableName() string { return "custom_kbmanager_operations" }

func (o *Operation) BeforeCreate(_ *gorm.DB) error {
	if o.ID == "" {
		o.ID = uuid.NewString()
	}
	if o.State == "" {
		o.State = OperationStatePreparing
	}
	return nil
}

func (o *Operation) Terminal() bool {
	if o == nil {
		return true
	}
	switch o.State {
	case OperationStateCompleted, OperationStateFailed, OperationStateDuplicate, OperationStateNoChange:
		return true
	default:
		return false
	}
}

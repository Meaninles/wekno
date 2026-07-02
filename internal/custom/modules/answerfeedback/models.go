package answerfeedback

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	FeedbackLike    = "like"
	FeedbackDislike = "dislike"
	FeedbackNone    = ""
)

// Feedback stores the current lightweight signal for one answer and actor.
type Feedback struct {
	ID                 string        `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID           uint64        `json:"tenant_id" gorm:"not null;index;uniqueIndex:idx_custom_answer_feedback_actor_msg"`
	UserID             string        `json:"user_id,omitempty" gorm:"type:varchar(64);index"`
	ActorKey           string        `json:"actor_key" gorm:"type:varchar(128);not null;uniqueIndex:idx_custom_answer_feedback_actor_msg"`
	SessionID          string        `json:"session_id" gorm:"type:varchar(36);not null;index"`
	RequestID          string        `json:"request_id,omitempty" gorm:"type:varchar(64);index"`
	AssistantMessageID string        `json:"assistant_message_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_answer_feedback_actor_msg"`
	Feedback           string        `json:"feedback" gorm:"type:varchar(16);not null"`
	Channel            string        `json:"channel,omitempty" gorm:"type:varchar(50);index"`
	Metadata           types.JSONMap `json:"metadata,omitempty" gorm:"type:jsonb"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

func (Feedback) TableName() string {
	return "custom_answer_feedbacks"
}

func (f *Feedback) BeforeCreate(tx *gorm.DB) error {
	if f.ID == "" {
		f.ID = uuid.New().String()
	}
	return nil
}

// RunSnapshot stores the delayed, extensible data record for future training datasets.
type RunSnapshot struct {
	ID                 string        `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID           uint64        `json:"tenant_id" gorm:"not null;index"`
	UserID             string        `json:"user_id,omitempty" gorm:"type:varchar(64);index"`
	SessionID          string        `json:"session_id" gorm:"type:varchar(36);not null;index"`
	RequestID          string        `json:"request_id,omitempty" gorm:"type:varchar(64);index"`
	UserMessageID      string        `json:"user_message_id,omitempty" gorm:"type:varchar(36);index"`
	AssistantMessageID string        `json:"assistant_message_id" gorm:"type:varchar(36);not null;uniqueIndex"`
	Channel            string        `json:"channel,omitempty" gorm:"type:varchar(50);index"`
	AgentID            string        `json:"agent_id,omitempty" gorm:"type:varchar(36);index"`
	AgentTenantID      uint64        `json:"agent_tenant_id,omitempty" gorm:"index"`
	AgentMode          string        `json:"agent_mode,omitempty" gorm:"type:varchar(50);index"`
	AgentType          string        `json:"agent_type,omitempty" gorm:"type:varchar(50);index"`
	ModelID            string        `json:"model_id,omitempty" gorm:"type:varchar(128);index"`
	UserQuery          string        `json:"user_query,omitempty" gorm:"type:text"`
	AssistantAnswer    string        `json:"assistant_answer,omitempty" gorm:"type:text"`
	Snapshot           types.JSONMap `json:"snapshot,omitempty" gorm:"type:jsonb"`
	CollectedAt        time.Time     `json:"collected_at" gorm:"index"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

func (RunSnapshot) TableName() string {
	return "custom_answer_run_snapshots"
}

func (r *RunSnapshot) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	return nil
}

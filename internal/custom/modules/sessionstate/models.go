package sessionstate

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ReadState stores the per-actor read watermark for one chat session.
type ReadState struct {
	ID        string    `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID  uint64    `json:"tenant_id" gorm:"not null;index;uniqueIndex:idx_custom_session_read_actor"`
	UserID    string    `json:"user_id,omitempty" gorm:"type:varchar(128);index"`
	ActorKey  string    `json:"actor_key" gorm:"type:varchar(160);not null;uniqueIndex:idx_custom_session_read_actor"`
	SessionID string    `json:"session_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_session_read_actor"`
	ReadAt    time.Time `json:"read_at" gorm:"not null;index"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ReadState) TableName() string {
	return "custom_session_read_states"
}

func (s *ReadState) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	return nil
}

type Status struct {
	SessionID              string     `json:"session_id"`
	LastAssistantMessageID string     `json:"last_assistant_message_id,omitempty"`
	LastAssistantAt        *time.Time `json:"last_assistant_at,omitempty"`
	IsRunning              bool       `json:"is_running"`
	Unread                 bool       `json:"unread"`
}

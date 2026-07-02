package scheduledchat

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	ScheduleTypeHourly  = "hourly"
	ScheduleTypeDaily   = "daily"
	ScheduleTypeWeekly  = "weekly"
	ScheduleTypeMonthly = "monthly"

	ConcurrencySkipIfRunning = "skip_if_running"

	RunStatusRunning = "running"
	RunStatusSuccess = "success"
	RunStatusFailed  = "failed"
	RunStatusSkipped = "skipped"

	TriggerSchedule = "schedule"
	TriggerManual   = "manual"

	SessionMarkerPrefix = "custom:scheduled-chat:"
	MessageChannel      = "schedule"
)

type Task struct {
	ID                string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID          uint64         `json:"tenant_id" gorm:"not null;index"`
	CreatedBy         string         `json:"created_by" gorm:"type:varchar(36);not null;index"`
	RunAsUserID       string         `json:"run_as_user_id" gorm:"type:varchar(36);not null;index"`
	Name              string         `json:"name" gorm:"type:varchar(255);not null"`
	Description       string         `json:"description" gorm:"type:text"`
	Enabled           bool           `json:"enabled" gorm:"not null;default:true;index"`
	AgentID           string         `json:"agent_id" gorm:"type:varchar(64);not null;index"`
	AgentNameSnapshot string         `json:"agent_name_snapshot" gorm:"type:varchar(255)"`
	ScheduleType      string         `json:"schedule_type" gorm:"type:varchar(16);not null;index"`
	Timezone          string         `json:"timezone" gorm:"type:varchar(64);not null;default:'Asia/Shanghai'"`
	Minute            int            `json:"minute" gorm:"not null;default:0"`
	Hour              int            `json:"hour" gorm:"not null;default:0"`
	Weekday           int            `json:"weekday" gorm:"not null;default:1"`      // 1=Monday ... 7=Sunday
	DayOfMonth        int            `json:"day_of_month" gorm:"not null;default:1"` // Invalid dates are skipped.
	PromptTemplate    string         `json:"prompt_template" gorm:"type:text;not null"`
	WebSearchEnabled  bool           `json:"web_search_enabled" gorm:"not null;default:false"`
	ConcurrencyPolicy string         `json:"concurrency_policy" gorm:"type:varchar(32);not null;default:'skip_if_running'"`
	NextRunAt         *time.Time     `json:"next_run_at" gorm:"index"`
	LastRunAt         *time.Time     `json:"last_run_at"`
	LastSuccessAt     *time.Time     `json:"last_success_at"`
	LastStatus        string         `json:"last_status" gorm:"type:varchar(32);default:''"`
	LastMessage       string         `json:"last_message" gorm:"type:text"`
	LastSessionID     string         `json:"last_session_id" gorm:"type:varchar(36);index"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	DeletedAt         gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Task) TableName() string {
	return "custom_scheduled_chat_tasks"
}

func (t *Task) BeforeCreate(tx *gorm.DB) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	return nil
}

type Run struct {
	ID                 string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TaskID             string         `json:"task_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_scheduled_chat_run_dedup"`
	TenantID           uint64         `json:"tenant_id" gorm:"not null;index"`
	RunAsUserID        string         `json:"run_as_user_id" gorm:"type:varchar(36);not null;index"`
	ScheduledAt        time.Time      `json:"scheduled_at" gorm:"not null;index;uniqueIndex:idx_custom_scheduled_chat_run_dedup"`
	TriggeredBy        string         `json:"triggered_by" gorm:"type:varchar(32);not null;default:'schedule'"`
	Status             string         `json:"status" gorm:"type:varchar(32);not null;index"`
	SessionID          string         `json:"session_id" gorm:"type:varchar(36);index"`
	UserMessageID      string         `json:"user_message_id" gorm:"type:varchar(36)"`
	AssistantMessageID string         `json:"assistant_message_id" gorm:"type:varchar(36)"`
	RenderedPrompt     string         `json:"rendered_prompt" gorm:"type:text"`
	ErrorMessage       string         `json:"error_message" gorm:"type:text"`
	StartedAt          *time.Time     `json:"started_at"`
	FinishedAt         *time.Time     `json:"finished_at"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	DeletedAt          gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Run) TableName() string {
	return "custom_scheduled_chat_runs"
}

func (r *Run) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	return nil
}

type TaskRequest struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	Enabled          *bool  `json:"enabled"`
	AgentID          string `json:"agent_id"`
	ScheduleType     string `json:"schedule_type"`
	Timezone         string `json:"timezone"`
	Minute           int    `json:"minute"`
	Hour             int    `json:"hour"`
	Weekday          int    `json:"weekday"`
	DayOfMonth       int    `json:"day_of_month"`
	PromptTemplate   string `json:"prompt_template"`
	WebSearchEnabled bool   `json:"web_search_enabled"`
}

type RenderPreviewRequest struct {
	PromptTemplate string `json:"prompt_template"`
	TaskName       string `json:"task_name"`
	AgentID        string `json:"agent_id"`
	Timezone       string `json:"timezone"`
}

type Variable struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type PromptTemplate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

package builtinagentpolicy

import "time"

// BuiltinAgentModelPolicy is the tenant-wide source of truth for the model
// bindings of one built-in agent. Model IDs are stored as tenant-local IDs so
// runtime calls never need to cross a tenant boundary for credentials.
type BuiltinAgentModelPolicy struct {
	TenantID               uint64 `gorm:"primaryKey"`
	AgentID                string `gorm:"primaryKey;type:varchar(64)"`
	ModelGroup             string `gorm:"type:varchar(32);not null"`
	ModelID                string `gorm:"type:varchar(64);not null"`
	RerankModelID          string `gorm:"type:varchar(64)"`
	QueryUnderstandModelID string `gorm:"type:varchar(64)"`
	VLMModelID             string `gorm:"type:varchar(64)"`
	ASRModelID             string `gorm:"type:varchar(64)"`
	UpdatedBy              string `gorm:"type:varchar(64)"`
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func (BuiltinAgentModelPolicy) TableName() string {
	return "custom_builtin_agent_model_policies"
}

// BuiltinAgentChatVisibility is a tenant-wide conversation-picker override.
// Missing rows intentionally mean "use the module default" so new built-ins
// can be introduced without a data migration.
type BuiltinAgentChatVisibility struct {
	TenantID  uint64 `gorm:"primaryKey"`
	AgentID   string `gorm:"primaryKey;type:varchar(64)"`
	Visible   bool   `gorm:"not null;default:false"`
	UpdatedBy string `gorm:"type:varchar(64)"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (BuiltinAgentChatVisibility) TableName() string {
	return "custom_builtin_agent_chat_visibility"
}

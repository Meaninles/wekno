package configcenter

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	ResourceModel       = "model"
	ResourceVectorStore = "vector_store"
	ResourceParser      = "parser_engine"
	ResourceStorage     = "storage_engine"
	ResourceWebSearch   = "web_search"
	ResourceMCP         = "mcp_service"

	TenantConfigResourceID = "__tenant__"
)

var ResourceTypes = []string{
	ResourceModel,
	ResourceVectorStore,
	ResourceParser,
	ResourceStorage,
	ResourceWebSearch,
	ResourceMCP,
}

type DefaultGrant struct {
	ID               string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	ResourceType     string         `json:"resource_type" gorm:"type:varchar(64);not null;uniqueIndex:idx_custom_default_grant"`
	SourceTenantID   uint64         `json:"source_tenant_id" gorm:"not null;uniqueIndex:idx_custom_default_grant"`
	SourceResourceID string         `json:"source_resource_id" gorm:"type:varchar(128);not null;uniqueIndex:idx_custom_default_grant"`
	Enabled          bool           `json:"enabled" gorm:"default:true;index"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	DeletedAt        gorm.DeletedAt `json:"-" gorm:"index"`
}

func (DefaultGrant) TableName() string {
	return "custom_config_default_grants"
}

func (g *DefaultGrant) BeforeCreate(tx *gorm.DB) error {
	if g.ID == "" {
		g.ID = uuid.New().String()
	}
	return nil
}

type UserGrant struct {
	ID               string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	UserID           string         `json:"user_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_custom_user_grant"`
	ResourceType     string         `json:"resource_type" gorm:"type:varchar(64);not null;uniqueIndex:idx_custom_user_grant"`
	SourceTenantID   uint64         `json:"source_tenant_id" gorm:"not null;uniqueIndex:idx_custom_user_grant"`
	SourceResourceID string         `json:"source_resource_id" gorm:"type:varchar(128);not null;uniqueIndex:idx_custom_user_grant"`
	Enabled          bool           `json:"enabled" gorm:"default:true;index"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	DeletedAt        gorm.DeletedAt `json:"-" gorm:"index"`
}

func (UserGrant) TableName() string {
	return "custom_config_user_grants"
}

func (g *UserGrant) BeforeCreate(tx *gorm.DB) error {
	if g.ID == "" {
		g.ID = uuid.New().String()
	}
	return nil
}

type ManagedCopy struct {
	ID               string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	UserID           string         `json:"user_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_custom_managed_copy"`
	TargetTenantID   uint64         `json:"target_tenant_id" gorm:"not null;uniqueIndex:idx_custom_managed_copy"`
	ResourceType     string         `json:"resource_type" gorm:"type:varchar(64);not null;uniqueIndex:idx_custom_managed_copy"`
	SourceTenantID   uint64         `json:"source_tenant_id" gorm:"not null;uniqueIndex:idx_custom_managed_copy"`
	SourceResourceID string         `json:"source_resource_id" gorm:"type:varchar(128);not null;uniqueIndex:idx_custom_managed_copy"`
	TargetResourceID string         `json:"target_resource_id" gorm:"type:varchar(128);not null"`
	SourceHash       string         `json:"source_hash" gorm:"type:varchar(64);not null;default:''"`
	Status           string         `json:"status" gorm:"type:varchar(32);not null;default:'active';index"`
	LastAppliedAt    *time.Time     `json:"last_applied_at"`
	LastError        string         `json:"last_error" gorm:"type:text"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	DeletedAt        gorm.DeletedAt `json:"-" gorm:"index"`
}

func (ManagedCopy) TableName() string {
	return "custom_config_managed_copies"
}

func (c *ManagedCopy) BeforeCreate(tx *gorm.DB) error {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	return nil
}

type ResourceRef struct {
	ResourceType     string `json:"resource_type"`
	SourceTenantID   uint64 `json:"source_tenant_id"`
	SourceResourceID string `json:"source_resource_id"`
}

type ResourceSummary struct {
	ResourceType   string `json:"resource_type"`
	ID             string `json:"id"`
	SourceTenantID uint64 `json:"source_tenant_id"`
	ConfigKey      string `json:"config_key,omitempty"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Enabled        bool   `json:"enabled"`
}

type UserSummary struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	TenantID uint64 `json:"tenant_id"`
	Active   bool   `json:"active"`
}

type ApplyResult struct {
	UsersApplied int      `json:"users_applied"`
	Resources    int      `json:"resources"`
	Errors       []string `json:"errors,omitempty"`
}

package iam

import (
	"log"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	ScheduleModeDaily  = "daily"
	ScheduleModeWeekly = "weekly"
	DefaultRunAt       = "03:10"
)

type SyncSetting struct {
	ID                 uint       `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Enabled            bool       `json:"enabled" gorm:"default:false"`
	BaseURL            string     `json:"base_url" gorm:"type:varchar(512)"`
	LoginClientID      string     `json:"login_client_id" gorm:"type:varchar(255)"`
	LoginClientSecret  string     `json:"login_client_secret,omitempty" gorm:"type:text"`
	SyncClientID       string     `json:"sync_client_id" gorm:"type:varchar(255)"`
	SyncClientSecret   string     `json:"sync_client_secret,omitempty" gorm:"type:text"`
	ScheduleMode       string     `json:"schedule_mode" gorm:"type:varchar(16);default:'daily'"`
	Weekdays           string     `json:"weekdays" gorm:"type:varchar(32);default:''"`
	RunAt              string     `json:"run_at" gorm:"type:varchar(8);default:'03:10'"`
	LastRunAt          *time.Time `json:"last_run_at"`
	LastStatus         string     `json:"last_status" gorm:"type:varchar(32);default:''"`
	LastMessage        string     `json:"last_message" gorm:"type:text"`
	LastRunTriggeredBy string     `json:"last_run_triggered_by" gorm:"type:varchar(64);default:''"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func (SyncSetting) TableName() string {
	return "custom_iam_sync_settings"
}

func (s *SyncSetting) BeforeSave(tx *gorm.DB) error {
	if key := utils.GetAESKey(); key != nil {
		if s.LoginClientSecret != "" {
			encrypted, err := utils.EncryptAESGCM(s.LoginClientSecret, key)
			if err != nil {
				return err
			}
			tx.Statement.SetColumn("login_client_secret", encrypted)
		}
		if s.SyncClientSecret != "" {
			encrypted, err := utils.EncryptAESGCM(s.SyncClientSecret, key)
			if err != nil {
				return err
			}
			tx.Statement.SetColumn("sync_client_secret", encrypted)
		}
	}
	return nil
}

func (s *SyncSetting) AfterFind(tx *gorm.DB) error {
	if plain, ok := utils.DecryptStoredSecretLenient(s.LoginClientSecret); ok {
		s.LoginClientSecret = plain
	} else {
		log.Printf("[crypto] custom IAM login_client_secret: decrypt failed (SYSTEM_AES_KEY missing/rotated?), treating as unconfigured")
		s.LoginClientSecret = ""
	}
	if plain, ok := utils.DecryptStoredSecretLenient(s.SyncClientSecret); ok {
		s.SyncClientSecret = plain
	} else {
		log.Printf("[crypto] custom IAM sync_client_secret: decrypt failed (SYSTEM_AES_KEY missing/rotated?), treating as unconfigured")
		s.SyncClientSecret = ""
	}
	return nil
}

type ExternalOrganization struct {
	ID                 string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	ExternalID         string         `json:"external_id" gorm:"type:varchar(128);uniqueIndex;not null"`
	ExternalBusinessID string         `json:"external_business_id" gorm:"type:varchar(128);index"`
	Code               string         `json:"code" gorm:"type:varchar(128);index"`
	Name               string         `json:"name" gorm:"type:varchar(255);not null"`
	ParentExternalID   string         `json:"parent_external_id" gorm:"type:varchar(128);index"`
	Disabled           bool           `json:"disabled" gorm:"default:false;index"`
	SubtreeUserCount   int64          `json:"subtree_user_count" gorm:"default:0"`
	Sequence           string         `json:"sequence" gorm:"type:varchar(128)"`
	ExternalUpdatedAt  string         `json:"external_updated_at" gorm:"type:varchar(64)"`
	Raw                string         `json:"raw" gorm:"type:jsonb"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	DeletedAt          gorm.DeletedAt `json:"-" gorm:"index"`
}

func (ExternalOrganization) TableName() string {
	return "custom_iam_organizations"
}

func (o *ExternalOrganization) BeforeCreate(tx *gorm.DB) error {
	if o.ID == "" {
		o.ID = uuid.New().String()
	}
	return nil
}

type ExternalUser struct {
	ID                     string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	ExternalID             string         `json:"external_id" gorm:"type:varchar(128);uniqueIndex;not null"`
	ExternalAccountID      string         `json:"external_account_id" gorm:"type:varchar(128);index"`
	Username               string         `json:"username" gorm:"type:varchar(255);index"`
	DisplayName            string         `json:"display_name" gorm:"type:varchar(255)"`
	OrganizationExternalID string         `json:"organization_external_id" gorm:"type:varchar(128);index"`
	Disabled               bool           `json:"disabled" gorm:"default:false;index"`
	AccessEnabled          bool           `json:"access_enabled" gorm:"default:false;index"`
	WeKnoraUserID          string         `json:"weknora_user_id" gorm:"column:weknora_user_id;type:varchar(36);index"`
	ExternalUpdatedAt      string         `json:"external_updated_at" gorm:"type:varchar(64)"`
	Raw                    string         `json:"raw" gorm:"type:jsonb"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
	DeletedAt              gorm.DeletedAt `json:"-" gorm:"index"`
}

func (ExternalUser) TableName() string {
	return "custom_iam_users"
}

func (u *ExternalUser) BeforeCreate(tx *gorm.DB) error {
	if u.ID == "" {
		u.ID = uuid.New().String()
	}
	return nil
}

type SpaceMemberCandidateOrganization struct {
	ExternalID        string `json:"external_id"`
	Name              string `json:"name"`
	ParentExternalID  string `json:"parent_external_id,omitempty"`
	HasChildren       bool   `json:"has_children"`
	UserCount         int64  `json:"user_count"`
	TenantCount       int64  `json:"tenant_count"`
	NodeType          string `json:"node_type,omitempty"`
	IAMExternalID     string `json:"iam_external_id,omitempty"`
	UserID            string `json:"user_id,omitempty"`
	Username          string `json:"username,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	Avatar            string `json:"avatar,omitempty"`
	TenantID          uint64 `json:"tenant_id,omitempty"`
	TenantName        string `json:"tenant_name,omitempty"`
	HasLocalUser      bool   `json:"has_local_user,omitempty"`
	AccessEnabled     bool   `json:"access_enabled,omitempty"`
	AlreadySelected   bool   `json:"already_selected,omitempty"`
	SelectionDisabled bool   `json:"selection_disabled,omitempty"`
}

type SpaceMemberCandidateUser struct {
	IAMExternalID             string `json:"iam_external_id"`
	UserID                    string `json:"user_id"`
	Username                  string `json:"username"`
	DisplayName               string `json:"display_name,omitempty"`
	Avatar                    string `json:"avatar,omitempty"`
	TenantID                  uint64 `json:"tenant_id"`
	TenantName                string `json:"tenant_name,omitempty"`
	IAMOrganizationExternalID string `json:"iam_organization_external_id,omitempty"`
	IAMOrganizationName       string `json:"iam_organization_name,omitempty"`
	HasLocalUser              bool   `json:"has_local_user"`
	AccessEnabled             bool   `json:"access_enabled"`
	AlreadySelected           bool   `json:"already_selected"`
	SelectionDisabled         bool   `json:"selection_disabled"`
}

type PendingSpaceMemberGrant struct {
	ID                string              `json:"id" gorm:"type:varchar(36);primaryKey"`
	OrganizationID    string              `json:"organization_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_custom_iam_pending_space_member"`
	IAMExternalUserID string              `json:"iam_external_user_id" gorm:"type:varchar(128);not null;uniqueIndex:idx_custom_iam_pending_space_member;index"`
	Role              types.OrgMemberRole `json:"role" gorm:"type:varchar(32);not null;default:'viewer'"`
	InvitedByUserID   string              `json:"invited_by_user_id" gorm:"type:varchar(36);default:''"`
	RedeemedAt        *time.Time          `json:"redeemed_at,omitempty" gorm:"index"`
	RedeemedTenantID  uint64              `json:"redeemed_tenant_id" gorm:"index"`
	RedeemedUserID    string              `json:"redeemed_user_id" gorm:"type:varchar(36);default:'';index"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
}

func (PendingSpaceMemberGrant) TableName() string {
	return "custom_iam_pending_space_member_grants"
}

func (g *PendingSpaceMemberGrant) BeforeCreate(tx *gorm.DB) error {
	if g.ID == "" {
		g.ID = uuid.New().String()
	}
	return nil
}

type SyncRun struct {
	ID            string           `json:"id" gorm:"type:varchar(36);primaryKey"`
	TriggeredBy   string           `json:"triggered_by" gorm:"type:varchar(64);not null"`
	Status        string           `json:"status" gorm:"type:varchar(32);not null;index"`
	Message       string           `json:"message" gorm:"type:text"`
	ScopeOrgID    string           `json:"scope_organization_external_id,omitempty" gorm:"type:varchar(128);index"`
	ScopeOrgName  string           `json:"scope_organization_name,omitempty" gorm:"type:varchar(255)"`
	OrgCount      int              `json:"org_count"`
	UserCount     int              `json:"user_count"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    *time.Time       `json:"finished_at"`
	CreatedUsers  int              `json:"created_users"`
	UpdatedUsers  int              `json:"updated_users"`
	DisabledUsers int              `json:"disabled_users"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Progress      *SyncRunProgress `json:"progress,omitempty" gorm:"-"`
}

func (SyncRun) TableName() string {
	return "custom_iam_sync_runs"
}

func (r *SyncRun) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	return nil
}

type SyncRunProgress struct {
	OrgCount       int        `json:"org_count"`
	UserCount      int        `json:"user_count"`
	CreatedUsers   int        `json:"created_users"`
	UpdatedUsers   int        `json:"updated_users"`
	DisabledUsers  int        `json:"disabled_users"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
}

package skillhub

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

type Skill struct {
	ID           string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID     uint64         `json:"tenant_id" gorm:"not null;index;uniqueIndex:idx_custom_skill_tenant_name"`
	CreatorID    string         `json:"creator_id" gorm:"type:varchar(36);not null;index"`
	Name         string         `json:"name" gorm:"type:varchar(64);not null;uniqueIndex:idx_custom_skill_global_name;uniqueIndex:idx_custom_skill_tenant_name"`
	Description  string         `json:"description" gorm:"type:text;not null"`
	Instructions string         `json:"instructions" gorm:"type:text;not null"`
	Enabled      bool           `json:"enabled" gorm:"not null;default:true;index"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `json:"-" gorm:"index"`
}

func (Skill) TableName() string {
	return "custom_skills"
}

func (s *Skill) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	return nil
}

type ProfessionalSkill struct {
	ID              string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID        uint64         `json:"tenant_id" gorm:"not null;index"`
	CreatorID       string         `json:"creator_id" gorm:"type:varchar(36);not null;index"`
	Name            string         `json:"name" gorm:"type:varchar(64);not null;uniqueIndex:idx_custom_professional_skill_name,where:deleted_at IS NULL"`
	Description     string         `json:"description" gorm:"type:text;not null"`
	ArchiveFileName string         `json:"archive_file_name" gorm:"type:varchar(255)"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`
}

func (ProfessionalSkill) TableName() string {
	return "custom_professional_skills"
}

func (s *ProfessionalSkill) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	return nil
}

type OrganizationShare struct {
	ID             string              `json:"id" gorm:"type:varchar(36);primaryKey"`
	SkillID        string              `json:"skill_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_skill_org_share"`
	OrganizationID string              `json:"organization_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_skill_org_share"`
	SharedByUserID string              `json:"shared_by_user_id" gorm:"type:varchar(36);not null"`
	SourceTenantID uint64              `json:"source_tenant_id" gorm:"not null;index"`
	Permission     types.OrgMemberRole `json:"permission" gorm:"type:varchar(32);not null;default:'viewer'"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	DeletedAt      gorm.DeletedAt      `json:"-" gorm:"index"`

	Skill        *Skill              `json:"skill,omitempty" gorm:"foreignKey:SkillID"`
	Organization *types.Organization `json:"organization,omitempty" gorm:"foreignKey:OrganizationID"`
	SharedByUser *types.User         `json:"shared_by_user,omitempty" gorm:"foreignKey:SharedByUserID"`
}

func (OrganizationShare) TableName() string {
	return "custom_skill_org_shares"
}

func (s *OrganizationShare) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	return nil
}

type UserShare struct {
	ID             string              `json:"id" gorm:"type:varchar(36);primaryKey"`
	SkillID        string              `json:"skill_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_skill_user_share"`
	TargetUserID   string              `json:"target_user_id" gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_skill_user_share"`
	SharedByUserID string              `json:"shared_by_user_id" gorm:"type:varchar(36);not null"`
	SourceTenantID uint64              `json:"source_tenant_id" gorm:"not null;index"`
	Permission     types.OrgMemberRole `json:"permission" gorm:"type:varchar(32);not null;default:'viewer'"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	DeletedAt      gorm.DeletedAt      `json:"-" gorm:"index"`

	Skill        *Skill      `json:"skill,omitempty" gorm:"foreignKey:SkillID"`
	TargetUser   *types.User `json:"target_user,omitempty" gorm:"foreignKey:TargetUserID"`
	SharedByUser *types.User `json:"shared_by_user,omitempty" gorm:"foreignKey:SharedByUserID"`
}

func (UserShare) TableName() string {
	return "custom_skill_user_shares"
}

func (s *UserShare) BeforeCreate(tx *gorm.DB) error {
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	return nil
}

type SkillRequest struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

type ShareOrganizationRequest struct {
	OrganizationID string              `json:"organization_id"`
	Permission     types.OrgMemberRole `json:"permission"`
}

type ShareUserRequest struct {
	UserID     string              `json:"user_id"`
	Permission types.OrgMemberRole `json:"permission"`
}

type SkillListItem struct {
	Skill
	IsMine           bool                `json:"is_mine"`
	ShareID          string              `json:"share_id,omitempty"`
	ShareType        string              `json:"share_type,omitempty"`
	OrganizationID   string              `json:"organization_id,omitempty"`
	OrganizationName string              `json:"organization_name,omitempty"`
	TargetUserID     string              `json:"target_user_id,omitempty"`
	TargetUsername   string              `json:"target_username,omitempty"`
	SharedByUserID   string              `json:"shared_by_user_id,omitempty"`
	SharedByUsername string              `json:"shared_by_username,omitempty"`
	SourceTenantID   uint64              `json:"source_tenant_id"`
	Permission       types.OrgMemberRole `json:"permission,omitempty"`
	SharedAt         *time.Time          `json:"shared_at,omitempty"`
}

type SkillShareList struct {
	OrganizationShares []SkillListItem `json:"organization_shares"`
	UserShares         []SkillListItem `json:"user_shares"`
}

type ProfessionalSkillListItem struct {
	ID               string              `json:"id,omitempty"`
	Name             string              `json:"name"`
	Description      string              `json:"description"`
	Kind             string              `json:"kind"`
	FileCount        int                 `json:"file_count"`
	Managed          bool                `json:"managed"`
	IsMine           bool                `json:"is_mine"`
	CanManage        bool                `json:"can_manage"`
	CanDownload      bool                `json:"can_download"`
	SystemReserved   bool                `json:"system_reserved"`
	ArchiveFileName  string              `json:"archive_file_name,omitempty"`
	ShareID          string              `json:"share_id,omitempty"`
	ShareType        string              `json:"share_type,omitempty"`
	OrganizationID   string              `json:"organization_id,omitempty"`
	OrganizationName string              `json:"organization_name,omitempty"`
	TargetUserID     string              `json:"target_user_id,omitempty"`
	TargetUsername   string              `json:"target_username,omitempty"`
	SharedByUserID   string              `json:"shared_by_user_id,omitempty"`
	SharedByUsername string              `json:"shared_by_username,omitempty"`
	SourceTenantID   uint64              `json:"source_tenant_id,omitempty"`
	Permission       types.OrgMemberRole `json:"permission,omitempty"`
	SharedAt         *time.Time          `json:"shared_at,omitempty"`
	UpdatedAt        *time.Time          `json:"updated_at,omitempty"`
}

type ProfessionalSkillShareList struct {
	OrganizationShares []ProfessionalSkillListItem `json:"organization_shares"`
	UserShares         []ProfessionalSkillListItem `json:"user_shares"`
}

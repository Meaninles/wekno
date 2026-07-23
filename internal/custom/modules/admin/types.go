package admin

import "time"

type SpaceKind string

const (
	SpaceKindTenant       SpaceKind = "tenant"
	SpaceKindOrganization SpaceKind = "organization"
)

type SpaceSummary struct {
	Kind          SpaceKind `json:"kind"`
	ID            string    `json:"id"`
	TenantID      uint64    `json:"tenant_id,omitempty"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	Status        string    `json:"status,omitempty"`
	OwnerUserID   string    `json:"owner_user_id,omitempty"`
	OwnerUsername string    `json:"owner_username,omitempty"`
	OwnerTenantID uint64    `json:"owner_tenant_id,omitempty"`
	MemberCount   int64     `json:"member_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type UserSummary struct {
	ID                        string    `json:"id"`
	Username                  string    `json:"username"`
	DisplayName               string    `json:"display_name,omitempty"`
	TenantID                  uint64    `json:"tenant_id"`
	TenantName                string    `json:"tenant_name,omitempty"`
	IsActive                  bool      `json:"is_active"`
	IsSystemAdmin             bool      `json:"is_system_admin"`
	HasLocalUser              bool      `json:"has_local_user"`
	AccessEnabled             bool      `json:"access_enabled"`
	IAMExternalID             string    `json:"iam_external_id,omitempty"`
	IAMUsername               string    `json:"iam_username,omitempty"`
	IAMDisplayName            string    `json:"iam_display_name,omitempty"`
	IAMOrganizationExternalID string    `json:"iam_organization_external_id,omitempty"`
	IAMOrganizationName       string    `json:"iam_organization_name,omitempty"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

type BulkUserActiveResult struct {
	Active              bool  `json:"active"`
	MatchedUsers        int64 `json:"matched_users"`
	UpdatedLocalUsers   int64 `json:"updated_local_users"`
	UpdatedIAMUsers     int64 `json:"updated_iam_users"`
	RevokedTokens       int64 `json:"revoked_tokens"`
	SkippedSelf         int64 `json:"skipped_self"`
	SkippedSystemAdmins int64 `json:"skipped_system_admins"`
}

type CreateLocalAccountRequest struct {
	Username    string `json:"username" binding:"required"`
	DisplayName string `json:"display_name"`
}

type CreateLocalAccountResult struct {
	User              UserSummary `json:"user"`
	TemporaryPassword string      `json:"temporary_password"`
	Warnings          []string    `json:"warnings,omitempty"`
}

package wikiaccess

import "time"

// UserPermission is an explicit, deny-by-default grant for selecting Wiki
// indexing. The absence of a row is intentionally equivalent to Enabled=false.
type UserPermission struct {
	UserID    string    `json:"user_id" gorm:"column:user_id;type:varchar(36);primaryKey"`
	Enabled   bool      `json:"wiki_enabled" gorm:"column:enabled;not null;default:false"`
	GrantedBy string    `json:"granted_by,omitempty" gorm:"column:granted_by;type:varchar(36);not null;default:''"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (UserPermission) TableName() string { return "custom_wiki_user_permissions" }

type UserSummary struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name,omitempty"`
	TenantID      uint64 `json:"tenant_id"`
	TenantName    string `json:"tenant_name,omitempty"`
	IsActive      bool   `json:"is_active"`
	IsSystemAdmin bool   `json:"is_system_admin"`
	WikiEnabled   bool   `json:"wiki_enabled"`
}

type UserPage struct {
	Users    []UserSummary `json:"users"`
	Total    int64         `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

type CurrentPermission struct {
	WikiEnabled bool `json:"wiki_enabled"`
}

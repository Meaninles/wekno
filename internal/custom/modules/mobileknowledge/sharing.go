package mobileknowledge

import (
	"context"
	"errors"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrKnowledgeBaseNotFound = errors.New("knowledge base not found")
	ErrShareTargetForbidden  = errors.New("knowledge base share targets forbidden")
)

const (
	defaultShareTargetPageSize = 20
	maxShareTargetPageSize     = 50
)

type ShareTarget struct {
	ID             string              `json:"id" gorm:"column:id"`
	Name           string              `json:"name" gorm:"column:name"`
	Description    string              `json:"description" gorm:"column:description"`
	Avatar         string              `json:"avatar" gorm:"column:avatar"`
	MyRole         types.OrgMemberRole `json:"my_role" gorm:"column:my_role"`
	IsOwner        bool                `json:"is_owner" gorm:"column:is_owner"`
	ShareID        string              `json:"share_id,omitempty" gorm:"column:share_id"`
	Permission     types.OrgMemberRole `json:"permission,omitempty" gorm:"column:permission"`
	CanRemove      bool                `json:"can_remove" gorm:"-"`
	SharedByUserID string              `json:"-" gorm:"column:shared_by_user_id"`
	SourceTenantID uint64              `json:"-" gorm:"column:source_tenant_id"`
}

type ShareTargetPage struct {
	Items    []ShareTarget `json:"items"`
	Mode     string        `json:"mode"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Total    int64         `json:"total"`
	HasMore  bool          `json:"has_more"`
}

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultShareTargetPageSize
	}
	if pageSize > maxShareTargetPageSize {
		pageSize = maxShareTargetPageSize
	}
	return page, pageSize
}

func likePattern(value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(value))
	return "%" + strings.ToLower(escaped) + "%"
}

// ListShareTargets performs permission filtering, search and pagination in the
// database. Source tenants see only organizations where they hold Editor+;
// receiving tenants see only existing shares they can govern as target-space
// Admins. No unfiltered organization list is materialized in the client.
func (s *Service) ListShareTargets(
	ctx context.Context,
	kbID, userID string,
	tenantID uint64,
	search string,
	page, pageSize int,
) (*ShareTargetPage, error) {
	page, pageSize = normalizePage(page, pageSize)

	var kb types.KnowledgeBase
	if err := s.db.WithContext(ctx).
		Select("id", "tenant_id", "creator_id").
		Where("id = ?", kbID).
		First(&kb).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKnowledgeBaseNotFound
		}
		return nil, err
	}

	role := types.TenantRoleFromContext(ctx)
	isSystemAdmin := types.IsSystemAdminFromContext(ctx)
	isSourceTenant := kb.TenantID == tenantID
	mode := "manage"
	if isSourceTenant {
		if !isSystemAdmin && kb.CreatorID != userID && !role.HasPermission(types.TenantRoleAdmin) {
			return nil, ErrShareTargetForbidden
		}
		mode = "share"
	}

	buildQuery := func() *gorm.DB {
		query := s.db.WithContext(ctx).
			Table("organizations").
			Joins("JOIN organization_tenant_members otm ON otm.organization_id = organizations.id").
			Where("organizations.deleted_at IS NULL").
			Where("otm.tenant_id = ?", tenantID)

		if isSourceTenant {
			query = query.
				Joins("LEFT JOIN kb_shares ON kb_shares.organization_id = organizations.id AND kb_shares.knowledge_base_id = ? AND kb_shares.deleted_at IS NULL", kbID).
				Where("otm.role IN ?", []types.OrgMemberRole{types.OrgRoleAdmin, types.OrgRoleEditor})
		} else {
			query = query.
				Joins("JOIN kb_shares ON kb_shares.organization_id = organizations.id AND kb_shares.knowledge_base_id = ? AND kb_shares.deleted_at IS NULL", kbID).
				Where("otm.role = ?", types.OrgRoleAdmin)
		}

		if strings.TrimSpace(search) != "" {
			query = query.Where("LOWER(organizations.name) LIKE ? ESCAPE '\\'", likePattern(search))
		}
		return query
	}

	var total int64
	if err := buildQuery().Count(&total).Error; err != nil {
		return nil, err
	}

	items := make([]ShareTarget, 0, pageSize)
	selectColumns := `organizations.id,
		organizations.name,
		organizations.description,
		organizations.avatar,
		otm.role AS my_role,
		CASE WHEN organizations.owner_tenant_id = ? THEN TRUE ELSE FALSE END AS is_owner,
		COALESCE(kb_shares.id, '') AS share_id,
		COALESCE(kb_shares.permission, '') AS permission,
		COALESCE(kb_shares.shared_by_user_id, '') AS shared_by_user_id,
		COALESCE(kb_shares.source_tenant_id, 0) AS source_tenant_id`
	if err := buildQuery().
		Select(selectColumns, tenantID).
		Order(clause.OrderBy{Expression: clause.Expr{
			SQL: `CASE WHEN organizations.owner_tenant_id = ? THEN 0
				WHEN otm.role = 'admin' THEN 1 ELSE 2 END,
				LOWER(organizations.name) ASC,
				organizations.id ASC`,
			Vars: []interface{}{tenantID},
		}}).
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Scan(&items).Error; err != nil {
		return nil, err
	}

	for index := range items {
		if items[index].ShareID == "" {
			continue
		}
		items[index].CanRemove = isSystemAdmin ||
			items[index].SharedByUserID == userID ||
			(items[index].SourceTenantID == tenantID && role.HasPermission(types.TenantRoleAdmin)) ||
			(!isSourceTenant && items[index].MyRole == types.OrgRoleAdmin)
	}

	return &ShareTargetPage{
		Items:    items,
		Mode:     mode,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
		HasMore:  int64(page*pageSize) < total,
	}, nil
}

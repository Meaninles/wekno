package admin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrQueryRequired         = errors.New("query or organization scope is required")
	ErrCannotDisableSelf     = errors.New("cannot disable your own account")
	ErrLastActiveSystemAdmin = errors.New("cannot disable the last active system administrator")
	ErrUserNotFound          = errors.New("user not found")
	ErrInvalidActiveState    = errors.New("active state is required")
)

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

func (s *Service) SearchSpaces(ctx context.Context, query string, limit int) ([]SpaceSummary, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []SpaceSummary{}, nil
	}
	limit = normalizeLimit(limit, 20, 50)
	pattern := likePattern(query)

	tenantRows, err := s.searchTenantSpaces(ctx, pattern, limit)
	if err != nil {
		return nil, err
	}
	orgRows, err := s.searchOrganizationSpaces(ctx, pattern, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SpaceSummary, 0, len(tenantRows)+len(orgRows))
	out = append(out, tenantRows...)
	out = append(out, orgRows...)
	return out, nil
}

func (s *Service) searchTenantSpaces(ctx context.Context, pattern string, limit int) ([]SpaceSummary, error) {
	type tenantRow struct {
		TenantID    uint64
		Name        string
		Description string
		Status      string
		CreatedAt   time.Time
		UpdatedAt   time.Time
	}
	var tenants []tenantRow
	if err := s.db.WithContext(ctx).
		Table("tenants AS t").
		Select("t.id AS tenant_id, t.name, t.description, t.status, t.created_at, t.updated_at").
		Where("t.deleted_at IS NULL").
		Where(`
			LOWER(COALESCE(t.name, '')) LIKE ? OR
			LOWER(COALESCE(t.description, '')) LIKE ? OR
			CAST(t.id AS TEXT) LIKE ?`,
			pattern, pattern, pattern).
		Order("t.created_at DESC").
		Limit(limit).
		Scan(&tenants).Error; err != nil {
		return nil, err
	}
	if len(tenants) == 0 {
		return []SpaceSummary{}, nil
	}

	tenantIDs := make([]uint64, 0, len(tenants))
	for _, row := range tenants {
		tenantIDs = append(tenantIDs, row.TenantID)
	}

	type ownerRow struct {
		TenantID uint64
		UserID   string
		Username string
	}
	var owners []ownerRow
	if err := s.db.WithContext(ctx).
		Table("users AS u").
		Select("u.tenant_id, u.id AS user_id, u.username").
		Where("u.deleted_at IS NULL AND u.tenant_id IN ?", tenantIDs).
		Order("u.tenant_id ASC, u.created_at ASC").
		Scan(&owners).Error; err != nil {
		return nil, err
	}
	ownerByTenant := make(map[uint64]ownerRow, len(owners))
	for _, owner := range owners {
		if _, ok := ownerByTenant[owner.TenantID]; !ok {
			ownerByTenant[owner.TenantID] = owner
		}
	}

	type countRow struct {
		TenantID uint64
		Count    int64
	}
	var counts []countRow
	if err := s.db.WithContext(ctx).
		Table("tenant_members").
		Select("tenant_id, COUNT(*) AS count").
		Where("deleted_at IS NULL AND status = ? AND tenant_id IN ?", types.TenantMemberStatusActive, tenantIDs).
		Group("tenant_id").
		Scan(&counts).Error; err != nil {
		return nil, err
	}
	countByTenant := make(map[uint64]int64, len(counts))
	for _, count := range counts {
		countByTenant[count.TenantID] = count.Count
	}

	out := make([]SpaceSummary, 0, len(tenants))
	for _, row := range tenants {
		owner := ownerByTenant[row.TenantID]
		out = append(out, SpaceSummary{
			Kind:          SpaceKindTenant,
			ID:            strconv.FormatUint(row.TenantID, 10),
			TenantID:      row.TenantID,
			Name:          row.Name,
			Description:   row.Description,
			Status:        row.Status,
			OwnerUserID:   owner.UserID,
			OwnerUsername: owner.Username,
			MemberCount:   countByTenant[row.TenantID],
			CreatedAt:     row.CreatedAt,
			UpdatedAt:     row.UpdatedAt,
		})
	}
	return out, nil
}

func (s *Service) searchOrganizationSpaces(ctx context.Context, pattern string, limit int) ([]SpaceSummary, error) {
	type orgRow struct {
		ID            string
		Name          string
		Description   string
		OwnerUserID   string
		OwnerTenantID uint64
		CreatedAt     time.Time
		UpdatedAt     time.Time
	}
	var orgs []orgRow
	if err := s.db.WithContext(ctx).
		Table("organizations AS o").
		Select("o.id, o.name, o.description, o.owner_id AS owner_user_id, o.owner_tenant_id, o.created_at, o.updated_at").
		Where("o.deleted_at IS NULL").
		Where(`
			LOWER(COALESCE(o.name, '')) LIKE ? OR
			LOWER(COALESCE(o.description, '')) LIKE ? OR
			LOWER(COALESCE(o.id, '')) LIKE ?`,
			pattern, pattern, pattern).
		Order("o.created_at DESC").
		Limit(limit).
		Scan(&orgs).Error; err != nil {
		return nil, err
	}
	if len(orgs) == 0 {
		return []SpaceSummary{}, nil
	}

	orgIDs := make([]string, 0, len(orgs))
	ownerIDs := make([]string, 0, len(orgs))
	for _, row := range orgs {
		orgIDs = append(orgIDs, row.ID)
		if strings.TrimSpace(row.OwnerUserID) != "" {
			ownerIDs = append(ownerIDs, row.OwnerUserID)
		}
	}

	type ownerRow struct {
		ID       string
		Username string
	}
	var owners []ownerRow
	if len(ownerIDs) > 0 {
		if err := s.db.WithContext(ctx).
			Table("users").
			Select("id, username").
			Where("deleted_at IS NULL AND id IN ?", ownerIDs).
			Scan(&owners).Error; err != nil {
			return nil, err
		}
	}
	ownerByID := make(map[string]string, len(owners))
	for _, owner := range owners {
		ownerByID[owner.ID] = owner.Username
	}

	type countRow struct {
		OrganizationID string
		Count          int64
	}
	var counts []countRow
	if err := s.db.WithContext(ctx).
		Table("organization_tenant_members").
		Select("organization_id, COUNT(*) AS count").
		Where("organization_id IN ?", orgIDs).
		Group("organization_id").
		Scan(&counts).Error; err != nil {
		return nil, err
	}
	countByOrg := make(map[string]int64, len(counts))
	for _, count := range counts {
		countByOrg[count.OrganizationID] = count.Count
	}

	out := make([]SpaceSummary, 0, len(orgs))
	for _, row := range orgs {
		out = append(out, SpaceSummary{
			Kind:          SpaceKindOrganization,
			ID:            row.ID,
			Name:          row.Name,
			Description:   row.Description,
			OwnerUserID:   row.OwnerUserID,
			OwnerUsername: ownerByID[row.OwnerUserID],
			OwnerTenantID: row.OwnerTenantID,
			MemberCount:   countByOrg[row.ID],
			CreatedAt:     row.CreatedAt,
			UpdatedAt:     row.UpdatedAt,
		})
	}
	return out, nil
}

func (s *Service) SearchUsers(ctx context.Context, query string, iamOrgExternalIDs []string, directOnly bool, limit int, iamExternalIDs ...[]string) ([]UserSummary, error) {
	query = strings.TrimSpace(query)
	orgScope := normalizeIDs(iamOrgExternalIDs)
	selectedIAMExternalIDs := []string{}
	if len(iamExternalIDs) > 0 {
		selectedIAMExternalIDs = normalizeIDs(iamExternalIDs[0])
	}
	var err error
	if !directOnly {
		orgScope, err = s.resolveIAMOrganizationScope(ctx, orgScope)
		if err != nil {
			return nil, err
		}
	}
	if query == "" && len(orgScope) == 0 && len(selectedIAMExternalIDs) == 0 {
		return []UserSummary{}, nil
	}
	maxLimit := 200
	if len(orgScope) > 0 {
		maxLimit = 1000
	}
	limit = normalizeLimit(limit, 50, maxLimit)

	var rows []UserSummary
	if query != "" || len(orgScope) > 0 {
		db := s.db.WithContext(ctx).
			Table("users AS u").
			Select(`
				u.id,
				u.username,
				u.display_name,
				u.tenant_id,
				COALESCE(t.name, '') AS tenant_name,
				u.is_active,
				u.is_system_admin,
				true AS has_local_user,
				CASE WHEN COALESCE(iu.external_id, '') <> '' THEN iu.access_enabled ELSE true END AS access_enabled,
				COALESCE(iu.external_id, '') AS iam_external_id,
				COALESCE(iu.username, '') AS iam_username,
				COALESCE(iu.display_name, '') AS iam_display_name,
				COALESCE(iu.organization_external_id, '') AS iam_organization_external_id,
				COALESCE(io.name, '') AS iam_organization_name,
				u.created_at,
				u.updated_at`).
			Joins("LEFT JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
			Joins("LEFT JOIN custom_iam_users AS iu ON iu.weknora_user_id = u.id AND iu.deleted_at IS NULL").
			Joins("LEFT JOIN custom_iam_organizations AS io ON io.external_id = iu.organization_external_id AND io.deleted_at IS NULL").
			Where("u.deleted_at IS NULL")

		if len(orgScope) > 0 {
			db = db.Where("iu.organization_external_id IN ?", orgScope)
		}
		if query != "" {
			pattern := likePattern(query)
			db = db.Where(`
				LOWER(COALESCE(u.username, '')) LIKE ? OR
				LOWER(COALESCE(u.display_name, '')) LIKE ? OR
				LOWER(COALESCE(u.id, '')) LIKE ? OR
				LOWER(COALESCE(t.name, '')) LIKE ? OR
				LOWER(COALESCE(iu.external_id, '')) LIKE ? OR
				LOWER(COALESCE(iu.username, '')) LIKE ? OR
				LOWER(COALESCE(iu.display_name, '')) LIKE ? OR
				LOWER(COALESCE(io.name, '')) LIKE ?`,
				pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern)
		}

		if err := db.Order("u.username ASC, u.created_at DESC").Limit(limit).Scan(&rows).Error; err != nil {
			return nil, err
		}
		mirrorRows, err := s.searchMirrorOnlyUsers(ctx, query, orgScope, limit)
		if err != nil {
			return nil, err
		}
		rows = append(rows, mirrorRows...)
	}
	selectedRows, err := s.searchUsersByIAMExternalIDs(ctx, selectedIAMExternalIDs, limit)
	if err != nil {
		return nil, err
	}
	rows = append(rows, selectedRows...)
	rows = dedupeUserSummaries(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (s *Service) searchUsersByIAMExternalIDs(ctx context.Context, externalIDs []string, limit int) ([]UserSummary, error) {
	if len(externalIDs) == 0 {
		return []UserSummary{}, nil
	}
	localRows, err := s.searchLinkedIAMUsersByExternalIDs(ctx, externalIDs, limit)
	if err != nil {
		return nil, err
	}
	mirrorRows, err := s.searchMirrorOnlyUsersByExternalIDs(ctx, externalIDs, limit)
	if err != nil {
		return nil, err
	}
	rows := append(localRows, mirrorRows...)
	return dedupeUserSummaries(rows), nil
}

func (s *Service) searchLinkedIAMUsersByExternalIDs(ctx context.Context, externalIDs []string, limit int) ([]UserSummary, error) {
	var rows []UserSummary
	if len(externalIDs) == 0 {
		return rows, nil
	}
	if err := s.db.WithContext(ctx).
		Table("users AS u").
		Select(`
			u.id,
			u.username,
			u.display_name,
			u.tenant_id,
			COALESCE(t.name, '') AS tenant_name,
			u.is_active,
			u.is_system_admin,
			true AS has_local_user,
			iu.access_enabled,
			iu.external_id AS iam_external_id,
			iu.username AS iam_username,
			iu.display_name AS iam_display_name,
			iu.organization_external_id AS iam_organization_external_id,
			COALESCE(io.name, '') AS iam_organization_name,
			u.created_at,
			u.updated_at`).
		Joins("JOIN custom_iam_users AS iu ON iu.weknora_user_id = u.id AND iu.deleted_at IS NULL").
		Joins("LEFT JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
		Joins("LEFT JOIN custom_iam_organizations AS io ON io.external_id = iu.organization_external_id AND io.deleted_at IS NULL").
		Where("u.deleted_at IS NULL").
		Where("iu.external_id IN ?", externalIDs).
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Service) searchMirrorOnlyUsersByExternalIDs(ctx context.Context, externalIDs []string, limit int) ([]UserSummary, error) {
	if len(externalIDs) == 0 {
		return []UserSummary{}, nil
	}
	var rows []UserSummary
	if err := s.db.WithContext(ctx).
		Table("custom_iam_users AS iu").
		Select(`
			iu.external_id AS id,
			iu.username,
			iu.display_name,
			0 AS tenant_id,
			false AS is_system_admin,
			iu.access_enabled AS is_active,
			false AS has_local_user,
			iu.access_enabled,
			iu.external_id AS iam_external_id,
			iu.username AS iam_username,
			iu.display_name AS iam_display_name,
			iu.organization_external_id AS iam_organization_external_id,
			COALESCE(io.name, '') AS iam_organization_name,
			iu.created_at,
			iu.updated_at`).
		Joins("LEFT JOIN users AS u ON u.id = iu.weknora_user_id AND u.deleted_at IS NULL").
		Joins("LEFT JOIN custom_iam_organizations AS io ON io.external_id = iu.organization_external_id AND io.deleted_at IS NULL").
		Where("iu.deleted_at IS NULL AND iu.disabled = ?", false).
		Where("iu.external_id IN ?", externalIDs).
		Where("(COALESCE(iu.weknora_user_id, '') = '' OR u.id IS NULL)").
		Where(`NOT EXISTS (
			SELECT 1 FROM users AS same_user
			WHERE same_user.deleted_at IS NULL
			  AND same_user.username = iu.username
		)`).
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].IAMExternalID == "" {
			rows[i].IAMExternalID = rows[i].ID
		}
		rows[i].ID = "iam:" + rows[i].IAMExternalID
	}
	return rows, nil
}

func dedupeUserSummaries(rows []UserSummary) []UserSummary {
	out := make([]UserSummary, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		key := strings.TrimSpace(row.ID)
		if key == "" && row.IAMExternalID != "" {
			key = "iam:" + row.IAMExternalID
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, row)
	}
	return out
}

func (s *Service) searchMirrorOnlyUsers(ctx context.Context, query string, orgScope []string, limit int) ([]UserSummary, error) {
	db := s.db.WithContext(ctx).
		Table("custom_iam_users AS iu").
		Select(`
			iu.external_id AS id,
			iu.username,
			iu.display_name,
			0 AS tenant_id,
			false AS is_system_admin,
			iu.access_enabled AS is_active,
			false AS has_local_user,
			iu.access_enabled,
			iu.external_id AS iam_external_id,
			iu.username AS iam_username,
			iu.display_name AS iam_display_name,
			iu.organization_external_id AS iam_organization_external_id,
			COALESCE(io.name, '') AS iam_organization_name,
			iu.created_at,
			iu.updated_at`).
		Joins("LEFT JOIN users AS u ON u.id = iu.weknora_user_id AND u.deleted_at IS NULL").
		Joins("LEFT JOIN custom_iam_organizations AS io ON io.external_id = iu.organization_external_id AND io.deleted_at IS NULL").
		Where("iu.deleted_at IS NULL AND iu.disabled = ?", false).
		Where("COALESCE(iu.external_id, '') <> ''").
		Where("(COALESCE(iu.weknora_user_id, '') = '' OR u.id IS NULL)")
	db = db.Where(`NOT EXISTS (
		SELECT 1 FROM users AS same_user
		WHERE same_user.deleted_at IS NULL
		  AND same_user.username = iu.username
	)`)

	if len(orgScope) > 0 {
		db = db.Where("iu.organization_external_id IN ?", orgScope)
	}
	if query != "" {
		pattern := likePattern(query)
		db = db.Where(`
			LOWER(COALESCE(iu.username, '')) LIKE ? OR
			LOWER(COALESCE(iu.external_id, '')) LIKE ? OR
			LOWER(COALESCE(iu.display_name, '')) LIKE ? OR
			LOWER(COALESCE(io.name, '')) LIKE ?`,
			pattern, pattern, pattern, pattern)
	}

	var rows []UserSummary
	if err := db.Order("COALESCE(iu.display_name, '') ASC, iu.username ASC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].IAMExternalID == "" {
			rows[i].IAMExternalID = rows[i].ID
		}
		rows[i].ID = "iam:" + rows[i].IAMExternalID
	}
	return rows, nil
}

func (s *Service) SetUserActive(ctx context.Context, userID string, active bool, actorID string) (*UserSummary, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, ErrUserNotFound
	}
	if strings.HasPrefix(userID, "iam:") {
		return s.setIAMUserAccessEnabled(ctx, strings.TrimPrefix(userID, "iam:"), active, actorID)
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user types.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", userID).
			First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return err
		}
		if !active {
			if user.ID == actorID {
				return ErrCannotDisableSelf
			}
			if user.IsSystemAdmin {
				var count int64
				if err := tx.Model(&types.User{}).
					Where("deleted_at IS NULL AND is_system_admin = ? AND is_active = ? AND id <> ?", true, true, user.ID).
					Count(&count).Error; err != nil {
					return err
				}
				if count == 0 {
					return ErrLastActiveSystemAdmin
				}
			}
		}

		if user.IsActive == active {
			return nil
		}
		if err := tx.Model(&types.User{}).
			Where("id = ?", user.ID).
			Updates(map[string]any{"is_active": active, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		if err := tx.Table("custom_iam_users").
			Where("weknora_user_id = ? AND deleted_at IS NULL", user.ID).
			Updates(map[string]any{"access_enabled": active, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		if !active {
			if err := tx.Model(&types.AuthToken{}).
				Where("user_id = ? AND is_revoked = ?", user.ID, false).
				Update("is_revoked", true).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	users, err := s.SearchUsers(ctx, userID, nil, true, 1)
	if err == nil {
		for _, row := range users {
			if row.ID == userID {
				return &row, nil
			}
		}
	}
	var row UserSummary
	if err := s.db.WithContext(ctx).
		Table("users AS u").
		Select("u.id, u.username, u.tenant_id, COALESCE(t.name, '') AS tenant_name, u.is_active, u.is_system_admin, u.created_at, u.updated_at").
		Joins("LEFT JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
		Where("u.id = ? AND u.deleted_at IS NULL", userID).
		Scan(&row).Error; err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, ErrUserNotFound
	}
	return &row, nil
}

func (s *Service) setIAMUserAccessEnabled(ctx context.Context, externalID string, active bool, actorID string) (*UserSummary, error) {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return nil, ErrUserNotFound
	}
	var localUserID string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		type iamUserRow struct {
			ExternalID    string
			WeKnoraUserID string
			Disabled      bool
		}
		var iamUser iamUserRow
		if err := tx.Table("custom_iam_users").
			Select("external_id, weknora_user_id, disabled").
			Where("external_id = ? AND deleted_at IS NULL", externalID).
			Scan(&iamUser).Error; err != nil {
			return err
		}
		if iamUser.ExternalID == "" {
			return ErrUserNotFound
		}
		if active && iamUser.Disabled {
			return ErrUserNotFound
		}
		localUserID = strings.TrimSpace(iamUser.WeKnoraUserID)
		if err := tx.Table("custom_iam_users").
			Where("external_id = ? AND deleted_at IS NULL", externalID).
			Updates(map[string]any{"access_enabled": active, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		if localUserID == "" {
			return nil
		}
		var user types.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", localUserID).
			First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if !active {
			if user.ID == actorID {
				return ErrCannotDisableSelf
			}
			if user.IsSystemAdmin {
				var count int64
				if err := tx.Model(&types.User{}).
					Where("deleted_at IS NULL AND is_system_admin = ? AND is_active = ? AND id <> ?", true, true, user.ID).
					Count(&count).Error; err != nil {
					return err
				}
				if count == 0 {
					return ErrLastActiveSystemAdmin
				}
			}
		}
		if err := tx.Model(&types.User{}).
			Where("id = ?", user.ID).
			Updates(map[string]any{"is_active": active, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		if !active {
			if err := tx.Model(&types.AuthToken{}).
				Where("user_id = ? AND is_revoked = ?", user.ID, false).
				Update("is_revoked", true).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	rows, err := s.SearchUsers(ctx, externalID, nil, true, 10)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.IAMExternalID == externalID || row.ID == "iam:"+externalID || row.ID == localUserID {
			return &row, nil
		}
	}
	return nil, ErrUserNotFound
}

func (s *Service) BatchSetUsersActive(ctx context.Context, query string, iamOrgExternalIDs []string, iamExternalIDs []string, directOnly bool, active bool, actorID string) (*BulkUserActiveResult, error) {
	query = strings.TrimSpace(query)
	orgScope := normalizeIDs(iamOrgExternalIDs)
	selectedIAMExternalIDs := normalizeIDs(iamExternalIDs)
	var err error
	if !directOnly {
		orgScope, err = s.resolveIAMOrganizationScope(ctx, orgScope)
		if err != nil {
			return nil, err
		}
	}
	if query == "" && len(orgScope) == 0 && len(selectedIAMExternalIDs) == 0 {
		return nil, ErrQueryRequired
	}

	result := &BulkUserActiveResult{Active: active}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		localIDs, err := s.batchMatchedLocalUserIDs(tx, query, orgScope, selectedIAMExternalIDs)
		if err != nil {
			return err
		}
		iamExternalIDs, linkedUserByExternalID, err := s.batchMatchedIAMExternalIDs(tx, query, orgScope, selectedIAMExternalIDs, active)
		if err != nil {
			return err
		}

		localIDSet := make(map[string]bool, len(localIDs)+len(linkedUserByExternalID))
		for _, id := range localIDs {
			localIDSet[id] = true
		}
		for _, userID := range linkedUserByExternalID {
			if userID != "" {
				localIDSet[userID] = true
			}
		}
		localIDs = mapKeys(localIDSet)

		skippedLocal := map[string]bool{}
		if !active && len(localIDs) > 0 {
			type userGuardRow struct {
				ID            string
				IsSystemAdmin bool
			}
			var guards []userGuardRow
			if err := tx.Table("users").
				Select("id, is_system_admin").
				Where("deleted_at IS NULL AND id IN ?", localIDs).
				Scan(&guards).Error; err != nil {
				return err
			}
			for _, guard := range guards {
				if guard.ID == actorID {
					skippedLocal[guard.ID] = true
					result.SkippedSelf++
					continue
				}
				if guard.IsSystemAdmin {
					skippedLocal[guard.ID] = true
					result.SkippedSystemAdmins++
				}
			}
		}

		updateLocalIDs := make([]string, 0, len(localIDs))
		for _, id := range localIDs {
			if id != "" && !skippedLocal[id] {
				updateLocalIDs = append(updateLocalIDs, id)
			}
		}
		if len(updateLocalIDs) > 0 {
			for _, chunk := range chunkStrings(updateLocalIDs, 1000) {
				db := tx.Model(&types.User{}).
					Where("deleted_at IS NULL AND id IN ?", chunk).
					Updates(map[string]any{"is_active": active, "updated_at": time.Now()})
				if db.Error != nil {
					return db.Error
				}
				result.UpdatedLocalUsers += db.RowsAffected
				if !active {
					revoked := tx.Model(&types.AuthToken{}).
						Where("user_id IN ? AND is_revoked = ?", chunk, false).
						Update("is_revoked", true)
					if revoked.Error != nil {
						return revoked.Error
					}
					result.RevokedTokens += revoked.RowsAffected
				}
			}
		}

		updateIAMExternalIDs := make([]string, 0, len(iamExternalIDs))
		for _, externalID := range iamExternalIDs {
			if linkedUserID := linkedUserByExternalID[externalID]; !active && linkedUserID != "" && skippedLocal[linkedUserID] {
				continue
			}
			updateIAMExternalIDs = append(updateIAMExternalIDs, externalID)
		}
		if len(updateIAMExternalIDs) > 0 {
			for _, chunk := range chunkStrings(updateIAMExternalIDs, 1000) {
				db := tx.Table("custom_iam_users").
					Where("deleted_at IS NULL AND external_id IN ?", chunk).
					Updates(map[string]any{"access_enabled": active, "updated_at": time.Now()})
				if db.Error != nil {
					return db.Error
				}
				result.UpdatedIAMUsers += db.RowsAffected
			}
		}

		result.MatchedUsers = int64(len(unionUserIdentities(updateLocalIDs, updateIAMExternalIDs)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) batchMatchedLocalUserIDs(tx *gorm.DB, query string, orgScope []string, selectedIAMExternalIDs []string) ([]string, error) {
	ids := map[string]bool{}
	if query != "" || len(orgScope) > 0 {
		db := tx.Table("users AS u").
			Distinct("u.id").
			Joins("LEFT JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
			Joins("LEFT JOIN custom_iam_users AS iu ON iu.weknora_user_id = u.id AND iu.deleted_at IS NULL").
			Joins("LEFT JOIN custom_iam_organizations AS io ON io.external_id = iu.organization_external_id AND io.deleted_at IS NULL").
			Where("u.deleted_at IS NULL")
		if len(orgScope) > 0 {
			db = db.Where("iu.organization_external_id IN ?", orgScope)
		}
		if query != "" {
			pattern := likePattern(query)
			db = db.Where(`
				LOWER(COALESCE(u.username, '')) LIKE ? OR
				LOWER(COALESCE(u.display_name, '')) LIKE ? OR
				LOWER(COALESCE(u.id, '')) LIKE ? OR
				LOWER(COALESCE(t.name, '')) LIKE ? OR
				LOWER(COALESCE(iu.external_id, '')) LIKE ? OR
				LOWER(COALESCE(iu.username, '')) LIKE ? OR
				LOWER(COALESCE(iu.display_name, '')) LIKE ? OR
				LOWER(COALESCE(io.name, '')) LIKE ?`,
				pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern)
		}
		var rows []string
		if err := db.Pluck("u.id", &rows).Error; err != nil {
			return nil, err
		}
		for _, id := range rows {
			if id != "" {
				ids[id] = true
			}
		}
	}
	if len(selectedIAMExternalIDs) > 0 {
		var rows []string
		if err := tx.Table("custom_iam_users").
			Where("deleted_at IS NULL AND external_id IN ? AND COALESCE(weknora_user_id, '') <> ''", selectedIAMExternalIDs).
			Pluck("weknora_user_id", &rows).Error; err != nil {
			return nil, err
		}
		for _, id := range rows {
			if id != "" {
				ids[id] = true
			}
		}
	}
	return mapKeys(ids), nil
}

func (s *Service) batchMatchedIAMExternalIDs(tx *gorm.DB, query string, orgScope []string, selectedIAMExternalIDs []string, active bool) ([]string, map[string]string, error) {
	externalIDs := map[string]bool{}
	linkedUserByExternalID := map[string]string{}
	addRows := func(rows []struct {
		ExternalID    string
		WeKnoraUserID string
	}) {
		for _, row := range rows {
			id := strings.TrimSpace(row.ExternalID)
			if id == "" {
				continue
			}
			externalIDs[id] = true
			linkedUserByExternalID[id] = strings.TrimSpace(row.WeKnoraUserID)
		}
	}
	if query != "" || len(orgScope) > 0 {
		db := tx.Table("custom_iam_users AS iu").
			Select("iu.external_id, iu.weknora_user_id").
			Joins("LEFT JOIN custom_iam_organizations AS io ON io.external_id = iu.organization_external_id AND io.deleted_at IS NULL").
			Where("iu.deleted_at IS NULL AND iu.disabled = ?", false).
			Where("COALESCE(iu.external_id, '') <> ''")
		if len(orgScope) > 0 {
			db = db.Where("iu.organization_external_id IN ?", orgScope)
		}
		if query != "" {
			pattern := likePattern(query)
			db = db.Where(`
				LOWER(COALESCE(iu.username, '')) LIKE ? OR
				LOWER(COALESCE(iu.external_id, '')) LIKE ? OR
				LOWER(COALESCE(iu.display_name, '')) LIKE ? OR
				LOWER(COALESCE(io.name, '')) LIKE ?`,
				pattern, pattern, pattern, pattern)
		}
		var rows []struct {
			ExternalID    string
			WeKnoraUserID string
		}
		if err := db.Scan(&rows).Error; err != nil {
			return nil, nil, err
		}
		addRows(rows)
	}
	if len(selectedIAMExternalIDs) > 0 {
		var rows []struct {
			ExternalID    string
			WeKnoraUserID string
		}
		db := tx.Table("custom_iam_users").
			Select("external_id, weknora_user_id").
			Where("deleted_at IS NULL AND external_id IN ?", selectedIAMExternalIDs)
		if active {
			db = db.Where("disabled = ?", false)
		}
		if err := db.Scan(&rows).Error; err != nil {
			return nil, nil, err
		}
		addRows(rows)
	}
	return mapKeys(externalIDs), linkedUserByExternalID, nil
}

func unionUserIdentities(localIDs []string, iamExternalIDs []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range localIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			out["local:"+id] = true
		}
	}
	for _, id := range iamExternalIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			out["iam:"+id] = true
		}
	}
	return out
}

func mapKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	return keys
}

func chunkStrings(values []string, size int) [][]string {
	if size <= 0 || len(values) <= size {
		return [][]string{values}
	}
	chunks := make([][]string, 0, (len(values)+size-1)/size)
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		chunks = append(chunks, values[start:end])
	}
	return chunks
}

func (s *Service) resolveIAMOrganizationScope(ctx context.Context, rootExternalIDs []string) ([]string, error) {
	roots := normalizeIDs(rootExternalIDs)
	if len(roots) == 0 {
		return nil, nil
	}
	type orgRow struct {
		ExternalID       string
		ParentExternalID string
	}
	var orgs []orgRow
	if err := s.db.WithContext(ctx).
		Table("custom_iam_organizations").
		Select("external_id, parent_external_id").
		Where("deleted_at IS NULL AND disabled = ?", false).
		Find(&orgs).Error; err != nil {
		return nil, err
	}
	childrenByParent := make(map[string][]string, len(orgs))
	for _, org := range orgs {
		id := strings.TrimSpace(org.ExternalID)
		if id == "" {
			continue
		}
		childrenByParent[strings.TrimSpace(org.ParentExternalID)] = append(childrenByParent[strings.TrimSpace(org.ParentExternalID)], id)
	}
	scope := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		scope = append(scope, id)
		queue = append(queue, childrenByParent[id]...)
	}
	return scope, nil
}

func normalizeIDs(values []string) []string {
	ids := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		ids = append(ids, value)
	}
	return ids
}

func normalizeLimit(value, fallback, max int) int {
	if value <= 0 {
		value = fallback
	}
	if value > max {
		return max
	}
	return value
}

func likePattern(query string) string {
	return "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
}

func parseLimit(raw string, fallback int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid limit")
	}
	return n, nil
}

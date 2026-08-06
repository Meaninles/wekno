package wikiaccess

import (
	"context"
	"errors"
	"strings"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrUserNotFound = errors.New("用户不存在")

const unavailableMessage = "Wiki功能暂时无法使用，请联系管理员开放wiki权限"

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).AutoMigrate(&UserPermission{})
}

// IsEnabled returns false for empty IDs and missing rows. This fail-closed
// contract is the central default for both the UI capability response and the
// server-side knowledge-base mutation guard.
func (s *Service) IsEnabled(ctx context.Context, userID string) (bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	var permission UserPermission
	err := s.db.WithContext(ctx).First(&permission, "user_id = ?", userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return permission.Enabled, nil
}

// GuardWikiSelection is registered into the native knowledge-base handler as
// a narrow hook. Infrastructure errors fail closed so callers cannot bypass
// the restriction during a database outage.
func (s *Service) GuardWikiSelection(ctx context.Context, userID string) error {
	enabled, err := s.IsEnabled(ctx, userID)
	if err != nil {
		return apperrors.NewForbiddenError(unavailableMessage)
	}
	if !enabled {
		return apperrors.NewForbiddenError(unavailableMessage)
	}
	return nil
}

func (s *Service) SearchUsers(ctx context.Context, query string, page int, pageSize int) (*UserPage, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	db := s.db.WithContext(ctx).
		Table("users AS u").
		Joins("LEFT JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
		Joins("LEFT JOIN custom_wiki_user_permissions AS w ON w.user_id = u.id").
		Where("u.deleted_at IS NULL")
	if query != "" {
		pattern := "%" + escapeLike(query) + "%"
		db = db.Where(`
			LOWER(COALESCE(u.username, '')) LIKE ? ESCAPE '!' OR
			LOWER(COALESCE(u.display_name, '')) LIKE ? ESCAPE '!' OR
			LOWER(COALESCE(u.id, '')) LIKE ? ESCAPE '!' OR
			LOWER(COALESCE(t.name, '')) LIKE ? ESCAPE '!'`,
			pattern, pattern, pattern, pattern)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, err
	}

	rows := make([]UserSummary, 0)
	if err := db.
		Select(`
			u.id,
			u.username,
			u.display_name,
			u.tenant_id,
			COALESCE(t.name, '') AS tenant_name,
			u.is_active,
			u.is_system_admin,
			COALESCE(w.enabled, false) AS wiki_enabled`).
		Order("u.username ASC, u.created_at DESC").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	return &UserPage{
		Users:    rows,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}

func (s *Service) SetUserPermission(
	ctx context.Context,
	userID string,
	enabled bool,
	actorUserID string,
) (*UserSummary, error) {
	userID = strings.TrimSpace(userID)
	actorUserID = strings.TrimSpace(actorUserID)
	if userID == "" {
		return nil, ErrUserNotFound
	}

	var count int64
	if err := s.db.WithContext(ctx).Model(&types.User{}).
		Where("id = ? AND deleted_at IS NULL", userID).
		Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrUserNotFound
	}

	now := time.Now()
	permission := UserPermission{
		UserID:    userID,
		Enabled:   enabled,
		GrantedBy: actorUserID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"enabled":    enabled,
			"granted_by": actorUserID,
			"updated_at": now,
		}),
	}).Create(&permission).Error; err != nil {
		return nil, err
	}

	result, err := s.SearchUsers(ctx, userID, 1, 2)
	if err != nil {
		return nil, err
	}
	for i := range result.Users {
		if result.Users[i].ID == userID {
			return &result.Users[i], nil
		}
	}
	return nil, ErrUserNotFound
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, "!", "!!")
	value = strings.ReplaceAll(value, "%", "!%")
	return strings.ReplaceAll(value, "_", "!_")
}

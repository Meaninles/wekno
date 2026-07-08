package iam

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	appservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type Service struct {
	db            *gorm.DB
	userService   interfaces.UserService
	httpClient    *http.Client
	provisionUser func(context.Context, *types.User) error

	mu        sync.Mutex
	scheduler *cron.Cron
	entryID   cron.EntryID
}

const syncRunTimeout = 30 * time.Minute

var (
	ErrIAMExternalUserNotFound       = errors.New("IAM external user not found")
	ErrPendingSpaceMemberGrantExists = errors.New("IAM pending space member grant already exists")
	ErrPendingSpaceMemberLimitFull   = errors.New("space member limit is full")
)

type SyncResult struct {
	OrgCount      int `json:"org_count"`
	UserCount     int `json:"user_count"`
	CreatedUsers  int `json:"created_users"`
	UpdatedUsers  int `json:"updated_users"`
	DisabledUsers int `json:"disabled_users"`
}

type PendingSpaceMemberGrantInput struct {
	OrganizationID    string
	IAMExternalUserID string
	Role              types.OrgMemberRole
	InvitedByUserID   string
}

type SyncScope struct {
	OrganizationExternalID string
}

type SSOConfigResponse struct {
	Success             bool   `json:"success"`
	Enabled             bool   `json:"enabled"`
	ProviderDisplayName string `json:"provider_display_name,omitempty"`
}

type SSOAuthURLResponse struct {
	Success          bool   `json:"success"`
	AuthorizationURL string `json:"authorization_url,omitempty"`
	State            string `json:"state,omitempty"`
	Message          string `json:"message,omitempty"`
}

type ssoStatePayload struct {
	Nonce            string `json:"nonce"`
	RedirectURI      string `json:"redirect_uri,omitempty"`
	FrontendRedirect string `json:"frontend_redirect,omitempty"`
}

type ssoTokenResponse struct {
	AccessToken string
	IDToken     string
}

func NewService(db *gorm.DB, userService interfaces.UserService) *Service {
	return &Service{
		db:          db,
		userService: userService,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *Service) SetProvisioner(fn func(context.Context, *types.User) error) {
	s.provisionUser = fn
}

func (s *Service) Migrate(ctx context.Context) error {
	if err := s.db.WithContext(ctx).AutoMigrate(&SyncSetting{}, &ExternalOrganization{}, &ExternalUser{}, &PendingSpaceMemberGrant{}, &SyncRun{}); err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Exec("ALTER TABLE custom_iam_users DROP COLUMN IF EXISTS email").Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Exec("ALTER TABLE users ADD COLUMN IF NOT EXISTS display_name varchar(255) NOT NULL DEFAULT ''").Error; err != nil {
		return err
	}
	var disabledLocalUserIDs []string
	if err := s.db.WithContext(ctx).
		Table("custom_iam_users").
		Where("deleted_at IS NULL AND access_enabled = ? AND COALESCE(weknora_user_id, '') <> ''", false).
		Pluck("weknora_user_id", &disabledLocalUserIDs).Error; err != nil {
		return err
	}
	if len(disabledLocalUserIDs) > 0 {
		if err := s.db.WithContext(ctx).Model(&types.User{}).
			Where("id IN ? AND is_system_admin = ?", disabledLocalUserIDs, false).
			Updates(map[string]any{"is_active": false, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
	}
	if err := s.cleanupMirrorOnlyUsersWithLocalUsers(ctx); err != nil {
		return err
	}
	return s.refreshOrganizationSubtreeUserCounts(ctx)
}

func (s *Service) GetSetting(ctx context.Context) (*SyncSetting, error) {
	var setting SyncSetting
	err := s.db.WithContext(ctx).First(&setting, "id = ?", 1).Error
	if err == nil {
		normalizeSetting(&setting)
		return &setting, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	setting = SyncSetting{
		ID:           1,
		Enabled:      false,
		ScheduleMode: ScheduleModeDaily,
		RunAt:        DefaultRunAt,
	}
	if err := s.db.WithContext(ctx).Create(&setting).Error; err != nil {
		return nil, err
	}
	return &setting, nil
}

func (s *Service) SaveSetting(ctx context.Context, next *SyncSetting) (*SyncSetting, error) {
	current, err := s.GetSetting(ctx)
	if err != nil {
		return nil, err
	}
	current.Enabled = next.Enabled
	current.BaseURL = strings.TrimRight(strings.TrimSpace(next.BaseURL), "/")
	current.LoginClientID = strings.TrimSpace(next.LoginClientID)
	current.SyncClientID = strings.TrimSpace(next.SyncClientID)
	current.ScheduleMode = strings.TrimSpace(next.ScheduleMode)
	current.Weekdays = normalizeWeekdays(next.Weekdays)
	current.RunAt = strings.TrimSpace(next.RunAt)
	if shouldReplaceSecret(next.LoginClientSecret) {
		current.LoginClientSecret = strings.TrimSpace(next.LoginClientSecret)
	}
	if shouldReplaceSecret(next.SyncClientSecret) {
		current.SyncClientSecret = strings.TrimSpace(next.SyncClientSecret)
	}
	normalizeSetting(current)
	if err := s.db.WithContext(ctx).Save(current).Error; err != nil {
		return nil, err
	}
	if err := s.ReloadSchedule(ctx); err != nil {
		logger.Warnf(ctx, "[custom iam] failed to reload schedule: %v", err)
	}
	return current, nil
}

func (s *Service) ListRuns(ctx context.Context, limit int) ([]SyncRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var runs []SyncRun
	if err := s.db.WithContext(ctx).Order("started_at DESC").Limit(limit).Find(&runs).Error; err != nil {
		return nil, err
	}
	for i := range runs {
		if runs[i].Status != "running" {
			continue
		}
		progress, err := s.runningSyncRunProgress(ctx, &runs[i])
		if err != nil {
			logger.Warnf(ctx, "[custom iam] failed to attach running sync progress %s: %v", runs[i].ID, err)
			continue
		}
		runs[i].Progress = progress
		runs[i].OrgCount = progress.OrgCount
		runs[i].UserCount = progress.UserCount
		runs[i].CreatedUsers = progress.CreatedUsers
		runs[i].UpdatedUsers = progress.UpdatedUsers
		runs[i].DisabledUsers = progress.DisabledUsers
	}
	return runs, nil
}

func (s *Service) runningSyncRunProgress(ctx context.Context, run *SyncRun) (*SyncRunProgress, error) {
	scopeIDs, err := s.runningSyncRunProgressScope(ctx, run.ScopeOrgID)
	if err != nil {
		return nil, err
	}

	progress := &SyncRunProgress{}
	if progress.OrgCount, err = countQueryRows(s.runningProgressOrganizationQuery(ctx, run, scopeIDs)); err != nil {
		return nil, err
	}
	if progress.UserCount, err = countQueryRows(s.runningProgressChangedUserQuery(ctx, run, scopeIDs)); err != nil {
		return nil, err
	}
	if progress.CreatedUsers, err = countQueryRows(s.runningProgressUserBaseQuery(ctx, scopeIDs).Where("created_at >= ?", run.StartedAt)); err != nil {
		return nil, err
	}
	if progress.UpdatedUsers, err = countQueryRows(s.runningProgressUserBaseQuery(ctx, scopeIDs).Where("updated_at >= ? AND created_at < ?", run.StartedAt, run.StartedAt)); err != nil {
		return nil, err
	}
	if progress.DisabledUsers, err = countQueryRows(s.runningProgressChangedUserQuery(ctx, run, scopeIDs).Where("disabled = ?", true)); err != nil {
		return nil, err
	}

	orgLast, err := newestUpdatedAt(s.runningProgressOrganizationQuery(ctx, run, scopeIDs))
	if err != nil {
		return nil, err
	}
	userLast, err := newestUpdatedAt(s.runningProgressChangedUserQuery(ctx, run, scopeIDs))
	if err != nil {
		return nil, err
	}
	progress.LastActivityAt = newestTime(orgLast, userLast)
	return progress, nil
}

func (s *Service) runningSyncRunProgressScope(ctx context.Context, rootExternalID string) ([]string, error) {
	rootExternalID = strings.TrimSpace(rootExternalID)
	if rootExternalID == "" {
		return nil, nil
	}

	var orgs []ExternalOrganization
	if err := s.db.WithContext(ctx).
		Select("external_id", "parent_external_id").
		Where("deleted_at IS NULL").
		Find(&orgs).Error; err != nil {
		return nil, err
	}

	childrenByParent := make(map[string][]string, len(orgs))
	for _, org := range orgs {
		orgID := strings.TrimSpace(org.ExternalID)
		if orgID == "" {
			continue
		}
		childrenByParent[strings.TrimSpace(org.ParentExternalID)] = append(childrenByParent[strings.TrimSpace(org.ParentExternalID)], orgID)
	}

	scope := make([]string, 0, len(orgs))
	seen := map[string]bool{}
	queue := []string{rootExternalID}
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
	if len(scope) == 0 {
		return []string{rootExternalID}, nil
	}
	return scope, nil
}

func (s *Service) runningProgressOrganizationQuery(ctx context.Context, run *SyncRun, scopeIDs []string) *gorm.DB {
	db := s.db.WithContext(ctx).
		Table("custom_iam_organizations").
		Where("deleted_at IS NULL").
		Where("updated_at >= ?", run.StartedAt)
	if len(scopeIDs) > 0 {
		db = db.Where("external_id IN ?", scopeIDs)
	}
	return db
}

func (s *Service) runningProgressUserBaseQuery(ctx context.Context, scopeIDs []string) *gorm.DB {
	db := s.db.WithContext(ctx).
		Table("custom_iam_users").
		Where("deleted_at IS NULL")
	if len(scopeIDs) > 0 {
		db = db.Where("organization_external_id IN ?", scopeIDs)
	}
	return db
}

func (s *Service) runningProgressChangedUserQuery(ctx context.Context, run *SyncRun, scopeIDs []string) *gorm.DB {
	return s.runningProgressUserBaseQuery(ctx, scopeIDs).
		Where("(created_at >= ? OR (updated_at >= ? AND created_at < ?))", run.StartedAt, run.StartedAt, run.StartedAt)
}

func countQueryRows(db *gorm.DB) (int, error) {
	var count int64
	if err := db.Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

func newestUpdatedAt(db *gorm.DB) (*time.Time, error) {
	var row struct {
		UpdatedAt time.Time `gorm:"column:updated_at"`
	}
	if err := db.Select("updated_at").Order("updated_at DESC").Limit(1).Scan(&row).Error; err != nil {
		return nil, err
	}
	if row.UpdatedAt.IsZero() {
		return nil, nil
	}
	return &row.UpdatedAt, nil
}

func newestTime(values ...*time.Time) *time.Time {
	var newest *time.Time
	for _, value := range values {
		if value == nil || value.IsZero() {
			continue
		}
		if newest == nil || value.After(*newest) {
			copied := *value
			newest = &copied
		}
	}
	return newest
}

func (s *Service) ListSpaceMemberCandidateOrganizations(ctx context.Context, spaceID, parentID, query string, limit int, includeUsers ...bool) ([]SpaceMemberCandidateOrganization, error) {
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return nil, fmt.Errorf("space_id is required")
	}
	return s.listOrganizations(ctx, parentID, query, limit, len(includeUsers) > 0 && includeUsers[0], spaceID)
}

func (s *Service) ListOrganizations(ctx context.Context, parentID, query string, limit int, includeUsers ...bool) ([]SpaceMemberCandidateOrganization, error) {
	return s.listOrganizations(ctx, parentID, query, limit, len(includeUsers) > 0 && includeUsers[0], "")
}

func (s *Service) listOrganizations(ctx context.Context, parentID, query string, limit int, includeUsers bool, spaceID string) ([]SpaceMemberCandidateOrganization, error) {
	parentID = strings.TrimSpace(parentID)
	limit = normalizeCandidateLimit(limit, 100, 1000)

	type organizationRow struct {
		ExternalID       string
		Name             string
		Code             string
		ParentExternalID string
		SubtreeUserCount int64
	}
	type childCountRow struct {
		ParentExternalID string
		ChildCount       int64
	}

	var orgs []organizationRow
	orgQuery := s.db.WithContext(ctx).
		Table("custom_iam_organizations AS io").
		Select("io.external_id, io.name, io.code, io.parent_external_id, io.subtree_user_count").
		Joins("LEFT JOIN custom_iam_organizations AS parent ON parent.external_id = io.parent_external_id AND parent.deleted_at IS NULL AND parent.disabled = ?", false).
		Where("io.deleted_at IS NULL AND io.disabled = ?", false).
		Where("io.external_id <> ''")
	if parentID == "" {
		orgQuery = orgQuery.Where("(COALESCE(io.parent_external_id, '') = '' OR parent.external_id IS NULL)")
	} else {
		orgQuery = orgQuery.Where("io.parent_external_id = ?", parentID)
	}
	if pattern := candidateSearchPattern(query); pattern != "" {
		orgQuery = orgQuery.Where(`
			LOWER(COALESCE(io.name, '')) LIKE ? OR
			LOWER(COALESCE(io.code, '')) LIKE ? OR
			LOWER(COALESCE(io.external_id, '')) LIKE ?`,
			pattern, pattern, pattern)
	}
	if err := orgQuery.
		Order("io.name ASC, io.external_id ASC").
		Limit(limit).
		Scan(&orgs).Error; err != nil {
		return nil, err
	}
	orgIDs := make([]string, 0, len(orgs))
	for _, org := range orgs {
		orgID := strings.TrimSpace(org.ExternalID)
		if orgID == "" {
			continue
		}
		orgIDs = append(orgIDs, orgID)
	}
	var childCounts []childCountRow
	if len(orgIDs) > 0 {
		if err := s.db.WithContext(ctx).
			Table("custom_iam_organizations AS io").
			Select("io.parent_external_id, COUNT(*) AS child_count").
			Where("io.deleted_at IS NULL AND io.disabled = ?", false).
			Where("io.parent_external_id IN ?", orgIDs).
			Group("io.parent_external_id").
			Scan(&childCounts).Error; err != nil {
			return nil, err
		}
	}
	childCountByOrg := make(map[string]int64, len(childCounts))
	for _, count := range childCounts {
		parentID := strings.TrimSpace(count.ParentExternalID)
		if parentID == "" {
			continue
		}
		childCountByOrg[parentID] = count.ChildCount
	}

	var err error
	userLeaves := []SpaceMemberCandidateOrganization{}
	if includeUsers && parentID != "" && candidateSearchPattern(query) == "" {
		userLeaves, err = s.listDirectOrganizationUserLeaves(ctx, parentID, limit, spaceID)
		if err != nil {
			return nil, err
		}
	}

	rows := make([]SpaceMemberCandidateOrganization, 0, len(orgs)+len(userLeaves))
	for _, org := range orgs {
		orgID := strings.TrimSpace(org.ExternalID)
		if orgID == "" {
			continue
		}
		rows = append(rows, SpaceMemberCandidateOrganization{
			ExternalID:       org.ExternalID,
			Name:             org.Name,
			ParentExternalID: org.ParentExternalID,
			HasChildren:      childCountByOrg[orgID] > 0 || (includeUsers && org.SubtreeUserCount > 0),
			UserCount:        org.SubtreeUserCount,
			TenantCount:      0,
			NodeType:         "organization",
		})
	}
	rows = append(rows, userLeaves...)
	return rows, nil
}

func (s *Service) listDirectOrganizationUserLeaves(ctx context.Context, parentID string, limit int, spaceID string) ([]SpaceMemberCandidateOrganization, error) {
	db := s.db.WithContext(ctx).
		Table("custom_iam_users AS iu").
		Select(`
			iu.external_id AS iam_external_id,
			iu.external_id,
			COALESCE(u.id, '') AS user_id,
			COALESCE(iu.display_name, '') AS name,
			iu.username,
			iu.display_name,
			COALESCE(u.avatar, '') AS avatar,
			COALESCE(u.tenant_id, 0) AS tenant_id,
			COALESCE(t.name, '') AS tenant_name,
			iu.organization_external_id AS parent_external_id,
			iu.access_enabled,
			CASE WHEN COALESCE(iu.weknora_user_id, '') <> '' AND u.id IS NOT NULL AND u.tenant_id > 0 THEN true ELSE false END AS has_local_user`).
		Joins("LEFT JOIN users AS u ON u.id = iu.weknora_user_id AND u.deleted_at IS NULL").
		Joins("LEFT JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
		Where("iu.deleted_at IS NULL AND iu.disabled = ?", false).
		Where("COALESCE(iu.external_id, '') <> ''").
		Where("iu.organization_external_id = ?", strings.TrimSpace(parentID)).
		Where(`NOT (
			COALESCE(iu.weknora_user_id, '') = ''
			AND EXISTS (
				SELECT 1 FROM users AS same_user
				WHERE same_user.deleted_at IS NULL
				  AND same_user.username = iu.username
			)
		)`)

	var rows []SpaceMemberCandidateOrganization
	if err := db.
		Order("COALESCE(iu.display_name, '') ASC, iu.username ASC, iu.external_id ASC").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].NodeType = "user"
		rows[i].HasChildren = false
		rows[i].UserCount = 0
		if strings.TrimSpace(rows[i].Name) == "" {
			rows[i].Name = firstNonEmpty(rows[i].DisplayName, rows[i].Username, rows[i].IAMExternalID, rows[i].ExternalID)
		}
	}
	if strings.TrimSpace(spaceID) != "" {
		existingTenantIDs, err := s.existingSpaceTenantIDs(ctx, spaceID)
		if err != nil {
			return nil, err
		}
		pendingExternalIDs, err := s.existingPendingSpaceMemberGrantExternalIDs(ctx, spaceID)
		if err != nil {
			return nil, err
		}
		markOrganizationUserLeavesSelectionState(rows, existingTenantIDs, pendingExternalIDs)
	}
	return rows, nil
}

func (s *Service) refreshOrganizationSubtreeUserCounts(ctx context.Context) error {
	type orgRow struct {
		ExternalID       string
		ParentExternalID string
	}
	type userCountRow struct {
		OrganizationExternalID string
		UserCount              int64
	}

	var orgs []orgRow
	if err := s.db.WithContext(ctx).
		Table("custom_iam_organizations").
		Select("external_id, parent_external_id").
		Where("deleted_at IS NULL AND disabled = ?", false).
		Where("COALESCE(external_id, '') <> ''").
		Find(&orgs).Error; err != nil {
		return err
	}

	direct := map[string]int64{}
	var directCounts []userCountRow
	if err := s.db.WithContext(ctx).
		Table("custom_iam_users").
		Select("organization_external_id, COUNT(DISTINCT external_id) AS user_count").
		Where("deleted_at IS NULL AND disabled = ?", false).
		Where("COALESCE(external_id, '') <> '' AND COALESCE(organization_external_id, '') <> ''").
		Where(`NOT (
			COALESCE(weknora_user_id, '') = ''
			AND EXISTS (
				SELECT 1 FROM users
				WHERE users.deleted_at IS NULL
				  AND users.username = custom_iam_users.username
			)
		)`).
		Group("organization_external_id").
		Scan(&directCounts).Error; err != nil {
		return err
	}
	for _, count := range directCounts {
		orgID := strings.TrimSpace(count.OrganizationExternalID)
		if orgID != "" {
			direct[orgID] = count.UserCount
		}
	}

	childrenByParent := make(map[string][]string, len(orgs))
	for _, org := range orgs {
		orgID := strings.TrimSpace(org.ExternalID)
		if orgID == "" {
			continue
		}
		childrenByParent[strings.TrimSpace(org.ParentExternalID)] = append(childrenByParent[strings.TrimSpace(org.ParentExternalID)], orgID)
	}

	total := map[string]int64{}
	visiting := map[string]bool{}
	var sum func(string) int64
	sum = func(orgID string) int64 {
		if value, ok := total[orgID]; ok {
			return value
		}
		if visiting[orgID] {
			return direct[orgID]
		}
		visiting[orgID] = true
		value := direct[orgID]
		for _, childID := range childrenByParent[orgID] {
			value += sum(childID)
		}
		visiting[orgID] = false
		total[orgID] = value
		return value
	}
	for _, org := range orgs {
		if orgID := strings.TrimSpace(org.ExternalID); orgID != "" {
			sum(orgID)
		}
	}

	if err := s.db.WithContext(ctx).
		Table("custom_iam_organizations").
		Where("deleted_at IS NULL").
		Update("subtree_user_count", 0).Error; err != nil {
		return err
	}
	for orgID, count := range total {
		if err := s.db.WithContext(ctx).
			Table("custom_iam_organizations").
			Where("external_id = ? AND deleted_at IS NULL", orgID).
			Update("subtree_user_count", count).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ListSpaceMemberCandidateUsers(ctx context.Context, spaceID, query string, iamOrgExternalIDs []string, directOnly bool, limit int) ([]SpaceMemberCandidateUser, error) {
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return nil, fmt.Errorf("space_id is required")
	}
	maxLimit := 200
	if len(iamOrgExternalIDs) > 0 {
		maxLimit = 10000
	}
	limit = normalizeCandidateLimit(limit, 50, maxLimit)

	existingTenantIDs, err := s.existingSpaceTenantIDs(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	pendingExternalIDs, err := s.existingPendingSpaceMemberGrantExternalIDs(ctx, spaceID)
	if err != nil {
		return nil, err
	}

	db := s.db.WithContext(ctx).
		Table("custom_iam_users AS iu").
		Select(`
			iu.external_id AS iam_external_id,
			COALESCE(iu.weknora_user_id, '') AS user_id,
			COALESCE(u.username, iu.username) AS username,
			COALESCE(NULLIF(u.display_name, ''), iu.display_name) AS display_name,
			COALESCE(u.avatar, '') AS avatar,
			COALESCE(u.tenant_id, 0) AS tenant_id,
			COALESCE(t.name, '') AS tenant_name,
			iu.organization_external_id AS iam_organization_external_id,
			io.name AS iam_organization_name,
			CASE WHEN COALESCE(iu.weknora_user_id, '') <> '' AND u.id IS NOT NULL AND u.tenant_id > 0 THEN true ELSE false END AS has_local_user,
			iu.access_enabled`).
		Joins("LEFT JOIN users AS u ON u.id = iu.weknora_user_id AND u.deleted_at IS NULL").
		Joins("LEFT JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
		Joins("LEFT JOIN custom_iam_organizations AS io ON io.external_id = iu.organization_external_id AND io.deleted_at IS NULL").
		Where("iu.deleted_at IS NULL AND iu.disabled = ?", false).
		Where("COALESCE(iu.external_id, '') <> ''")
	db = db.Where(`NOT (
		COALESCE(iu.weknora_user_id, '') = ''
		AND EXISTS (
			SELECT 1 FROM users AS same_user
			WHERE same_user.deleted_at IS NULL
			  AND same_user.username = iu.username
		)
	)`)

	orgScope := normalizeIAMOrganizationIDs(iamOrgExternalIDs)
	if !directOnly {
		orgScope, err = s.resolveIAMOrganizationScope(ctx, iamOrgExternalIDs)
		if err != nil {
			return nil, err
		}
	}
	if len(orgScope) > 0 {
		db = db.Where("iu.organization_external_id IN ?", orgScope)
	}
	if pattern := candidateSearchPattern(query); pattern != "" {
		db = db.Where(`
			LOWER(COALESCE(iu.username, '')) LIKE ? OR
			LOWER(COALESCE(iu.display_name, '')) LIKE ? OR
			LOWER(COALESCE(u.display_name, '')) LIKE ? OR
			LOWER(COALESCE(u.username, '')) LIKE ? OR
			LOWER(COALESCE(t.name, '')) LIKE ? OR
			LOWER(COALESCE(io.name, '')) LIKE ?`,
			pattern, pattern, pattern, pattern, pattern, pattern)
	}

	var rows []SpaceMemberCandidateUser
	if err := db.
		Order("COALESCE(io.name, '') ASC, COALESCE(iu.display_name, '') ASC, COALESCE(u.username, iu.username) ASC").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	markCandidateUsersSelectionState(rows, existingTenantIDs, pendingExternalIDs)
	return rows, nil
}

func markOrganizationUserLeavesSelectionState(rows []SpaceMemberCandidateOrganization, existingTenantIDs []uint64, pendingExternalIDs []string) {
	tenantSet := uint64Set(existingTenantIDs)
	externalIDSet := stringSet(pendingExternalIDs)
	for i := range rows {
		if rows[i].NodeType != "user" {
			continue
		}
		if (rows[i].TenantID > 0 && tenantSet[rows[i].TenantID]) || externalIDSet[strings.TrimSpace(rows[i].IAMExternalID)] {
			rows[i].AlreadySelected = true
			rows[i].SelectionDisabled = true
		}
	}
}

func markCandidateUsersSelectionState(rows []SpaceMemberCandidateUser, existingTenantIDs []uint64, pendingExternalIDs []string) {
	tenantSet := uint64Set(existingTenantIDs)
	externalIDSet := stringSet(pendingExternalIDs)
	for i := range rows {
		if (rows[i].TenantID > 0 && tenantSet[rows[i].TenantID]) || externalIDSet[strings.TrimSpace(rows[i].IAMExternalID)] {
			rows[i].AlreadySelected = true
			rows[i].SelectionDisabled = true
		}
	}
}

func uint64Set(values []uint64) map[uint64]bool {
	set := make(map[uint64]bool, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		set[value] = true
	}
	return set
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		set[value] = true
	}
	return set
}

func normalizeIAMOrganizationIDs(externalIDs []string) []string {
	ids := make([]string, 0, len(externalIDs))
	seen := map[string]bool{}
	for _, id := range externalIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func (s *Service) resolveIAMOrganizationScope(ctx context.Context, rootExternalIDs []string) ([]string, error) {
	roots := normalizeIAMOrganizationIDs(rootExternalIDs)
	if len(roots) == 0 {
		return nil, nil
	}

	var orgs []ExternalOrganization
	if err := s.db.WithContext(ctx).
		Select("external_id", "parent_external_id").
		Where("deleted_at IS NULL AND disabled = ?", false).
		Find(&orgs).Error; err != nil {
		return nil, err
	}

	childrenByParent := make(map[string][]string, len(orgs))
	for _, org := range orgs {
		if strings.TrimSpace(org.ExternalID) == "" {
			continue
		}
		parent := strings.TrimSpace(org.ParentExternalID)
		childrenByParent[parent] = append(childrenByParent[parent], org.ExternalID)
	}

	scope := make([]string, 0, len(roots))
	seen := map[string]bool{}
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

func (s *Service) existingSpaceTenantIDs(ctx context.Context, spaceID string) ([]uint64, error) {
	var members []types.OrganizationTenantMember
	if err := s.db.WithContext(ctx).
		Where("organization_id = ?", spaceID).
		Find(&members).Error; err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(members))
	seen := make(map[uint64]bool, len(members))
	for _, member := range members {
		if member.TenantID == 0 || seen[member.TenantID] {
			continue
		}
		seen[member.TenantID] = true
		ids = append(ids, member.TenantID)
	}
	return ids, nil
}

func (s *Service) existingPendingSpaceMemberGrantExternalIDs(ctx context.Context, spaceID string) ([]string, error) {
	var grants []PendingSpaceMemberGrant
	if err := s.db.WithContext(ctx).
		Select("iam_external_user_id").
		Where("organization_id = ? AND redeemed_at IS NULL", strings.TrimSpace(spaceID)).
		Find(&grants).Error; err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(grants))
	seen := make(map[string]bool, len(grants))
	for _, grant := range grants {
		id := strings.TrimSpace(grant.IAMExternalUserID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

func normalizeCandidateLimit(limit, fallback, max int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > max {
		return max
	}
	return limit
}

func candidateSearchPattern(query string) string {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return ""
	}
	return "%" + query + "%"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "sqlstate 23505")
}

func (s *Service) RunSync(ctx context.Context, triggeredBy string, scopes ...SyncScope) (*SyncRun, error) {
	if running, ok, err := s.currentRunningRun(ctx); err != nil {
		return nil, err
	} else if ok {
		return running, nil
	}
	scope := SyncScope{}
	if len(scopes) > 0 {
		scope = scopes[0]
	}

	run := &SyncRun{
		TriggeredBy: strings.TrimSpace(triggeredBy),
		Status:      "running",
		StartedAt:   time.Now(),
		ScopeOrgID:  strings.TrimSpace(scope.OrganizationExternalID),
	}
	if run.TriggeredBy == "" {
		run.TriggeredBy = "manual"
	}
	if run.ScopeOrgID != "" {
		run.TriggeredBy = run.TriggeredBy + ":organization"
		var org ExternalOrganization
		if err := s.db.WithContext(ctx).Select("name").First(&org, "external_id = ?", run.ScopeOrgID).Error; err == nil {
			run.ScopeOrgName = org.Name
		}
	}
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return nil, err
	}

	if err := s.db.WithContext(ctx).Model(&SyncSetting{}).Where("id = ?", 1).Updates(map[string]any{
		"last_run_at":           run.StartedAt,
		"last_status":           run.Status,
		"last_message":          "running",
		"last_run_triggered_by": run.TriggeredBy,
	}).Error; err != nil {
		logger.Warnf(ctx, "[custom iam] failed to update running sync status: %v", err)
	}

	go s.executeRun(run.ID)
	return run, nil
}

func (s *Service) currentRunningRun(ctx context.Context) (*SyncRun, bool, error) {
	var run SyncRun
	err := s.db.WithContext(ctx).Where("status = ?", "running").Order("started_at DESC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if time.Since(run.StartedAt) <= syncRunTimeout {
		return &run, true, nil
	}
	now := time.Now()
	run.FinishedAt = &now
	run.Status = "failed"
	run.Message = "sync task timed out"
	if err := s.db.WithContext(ctx).Save(&run).Error; err != nil {
		return nil, false, err
	}
	return nil, false, nil
}

func (s *Service) executeRun(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), syncRunTimeout)
	defer cancel()

	var run SyncRun
	if err := s.db.WithContext(ctx).First(&run, "id = ?", runID).Error; err != nil {
		logger.Errorf(ctx, "[custom iam] failed to load sync run %s: %v", runID, err)
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			s.finishRun(ctx, &run, nil, fmt.Errorf("sync panic: %v", recovered))
		}
	}()

	setting, err := s.GetSetting(ctx)
	if err != nil {
		s.finishRun(ctx, &run, nil, err)
		return
	}
	result, syncErr := s.syncOnce(ctx, setting, SyncScope{OrganizationExternalID: run.ScopeOrgID})
	if syncErr == nil {
		countCtx, countCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := s.refreshOrganizationSubtreeUserCounts(countCtx); err != nil {
			logger.Warnf(countCtx, "[custom iam] failed to refresh organization subtree user counts: %v", err)
		}
		countCancel()
	}
	s.finishRun(ctx, &run, result, syncErr)
}

func (s *Service) finishRun(ctx context.Context, run *SyncRun, result *SyncResult, syncErr error) {
	writeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ctx == nil {
		ctx = writeCtx
	}
	now := time.Now()
	run.FinishedAt = &now
	if syncErr != nil {
		run.Status = "failed"
		run.Message = syncErr.Error()
	} else {
		if result == nil {
			result = &SyncResult{}
		}
		run.Status = "success"
		run.OrgCount = result.OrgCount
		run.UserCount = result.UserCount
		run.CreatedUsers = result.CreatedUsers
		run.UpdatedUsers = result.UpdatedUsers
		run.DisabledUsers = result.DisabledUsers
		run.Message = "ok"
	}
	if err := s.db.WithContext(writeCtx).Save(run).Error; err != nil {
		logger.Errorf(ctx, "[custom iam] failed to save sync run %s: %v", run.ID, err)
		return
	}
	if err := s.db.WithContext(writeCtx).Model(&SyncSetting{}).Where("id = ?", 1).Updates(map[string]any{
		"last_run_at":           now,
		"last_status":           run.Status,
		"last_message":          run.Message,
		"last_run_triggered_by": run.TriggeredBy,
	}).Error; err != nil {
		logger.Warnf(ctx, "[custom iam] failed to update last sync status: %v", err)
	}
}

func (s *Service) GetSSOConfig(ctx context.Context) (*SSOConfigResponse, error) {
	setting, err := s.GetSetting(ctx)
	if err != nil {
		return nil, err
	}
	enabled := setting.BaseURL != "" && setting.LoginClientID != "" && setting.LoginClientSecret != ""
	return &SSOConfigResponse{
		Success:             true,
		Enabled:             enabled,
		ProviderDisplayName: "统一身份认证",
	}, nil
}

func (s *Service) GetSSOAuthorizationURL(ctx context.Context, redirectURI string, frontendRedirect ...string) (*SSOAuthURLResponse, error) {
	setting, err := s.GetSetting(ctx)
	if err != nil {
		return nil, err
	}
	if setting.BaseURL == "" || setting.LoginClientID == "" || setting.LoginClientSecret == "" {
		return nil, fmt.Errorf("IAM login base_url, client_id and client_secret are required")
	}
	if strings.TrimSpace(redirectURI) == "" {
		return nil, fmt.Errorf("redirect_uri is required")
	}
	payload := ssoStatePayload{Nonce: uuid.NewString(), RedirectURI: redirectURI}
	if len(frontendRedirect) > 0 {
		payload.FrontendRedirect = strings.TrimSpace(frontendRedirect[0])
	}
	state, err := encodeSSOState(payload)
	if err != nil {
		return nil, err
	}
	authURL, err := url.Parse(setting.BaseURL + "/idp/authCenter/authenticate")
	if err != nil {
		return nil, err
	}
	query := authURL.Query()
	query.Set("client_id", setting.LoginClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	authURL.RawQuery = query.Encode()
	return &SSOAuthURLResponse{
		Success:          true,
		AuthorizationURL: authURL.String(),
		State:            state,
	}, nil
}

func (s *Service) LoginWithSSO(ctx context.Context, code, state string) (*types.OIDCCallbackResponse, error) {
	setting, err := s.GetSetting(ctx)
	if err != nil {
		return nil, err
	}
	if setting.BaseURL == "" || setting.LoginClientID == "" || setting.LoginClientSecret == "" {
		return nil, fmt.Errorf("IAM login is not configured")
	}
	decoded, err := decodeSSOState(state)
	if err != nil {
		return nil, err
	}
	tokenResp, err := s.exchangeSSOToken(ctx, setting, code, decoded.RedirectURI)
	if err != nil {
		return nil, err
	}
	claims, err := s.resolveSSOUserInfo(ctx, setting, tokenResp)
	if err != nil {
		return nil, err
	}
	ext := externalUserFromClaims(claims)
	ext, allowed, err := s.prepareExternalUserForLogin(ctx, ext)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return &types.OIDCCallbackResponse{Success: false, Message: appservice.DisabledUserLoginMessage}, nil
	}
	user, err := s.ensureLocalUser(ctx, ext)
	if err != nil {
		return nil, err
	}
	if !user.IsActive {
		return &types.OIDCCallbackResponse{Success: false, Message: appservice.DisabledUserLoginMessage}, nil
	}
	ext.WeKnoraUserID = user.ID
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "external_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"external_account_id", "username", "display_name", "weknora_user_id", "access_enabled", "updated_at",
		}),
	}).Create(&ext).Error; err != nil {
		return nil, err
	}
	if err := s.RedeemPendingSpaceMemberGrants(ctx, ext.ExternalID, user); err != nil {
		logger.Warnf(ctx, "[custom iam] failed to redeem pending space member grants for %s: %v", user.Username, err)
	}
	accessToken, refreshToken, err := s.userService.GenerateTokens(ctx, user)
	if err != nil {
		return nil, err
	}
	var tenant types.Tenant
	if err := s.db.WithContext(ctx).First(&tenant, "id = ?", user.TenantID).Error; err != nil {
		return nil, err
	}
	return &types.OIDCCallbackResponse{
		Success:      true,
		Message:      "Login successful",
		User:         user,
		Tenant:       &tenant,
		Memberships:  s.userService.BuildLoginMemberships(ctx, user, &tenant),
		Token:        accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *Service) prepareExternalUserForLogin(ctx context.Context, ext ExternalUser) (ExternalUser, bool, error) {
	if strings.TrimSpace(ext.ExternalID) == "" {
		return ext, false, fmt.Errorf("IAM user missing external id")
	}
	var existing ExternalUser
	err := s.db.WithContext(ctx).
		Where("external_id = ? AND deleted_at IS NULL", ext.ExternalID).
		First(&existing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return ext, false, err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) && strings.TrimSpace(ext.Username) != "" {
		err = s.db.WithContext(ctx).
			Where("username = ? AND deleted_at IS NULL", ext.Username).
			First(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return ext, false, err
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if localUser, localErr := s.localUserByUsername(ctx, ext.Username); localErr != nil {
			return ext, false, localErr
		} else if localUser != nil {
			ext.WeKnoraUserID = localUser.ID
			ext.AccessEnabled = localUser.IsActive
		} else {
			ext.AccessEnabled = false
		}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "external_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"external_account_id", "username", "display_name", "weknora_user_id", "access_enabled", "raw", "updated_at",
			}),
		}).Create(&ext).Error; err != nil {
			return ext, false, err
		}
		return ext, !ext.Disabled && ext.AccessEnabled, nil
	}

	ext.ID = existing.ID
	ext.ExternalID = existing.ExternalID
	ext.WeKnoraUserID = existing.WeKnoraUserID
	ext.AccessEnabled = existing.AccessEnabled
	if strings.TrimSpace(ext.WeKnoraUserID) == "" {
		if localUser, localErr := s.localUserByUsername(ctx, ext.Username); localErr != nil {
			return ext, false, localErr
		} else if localUser != nil {
			ext.WeKnoraUserID = localUser.ID
			ext.AccessEnabled = localUser.IsActive
		}
	}
	ext.Disabled = existing.Disabled
	if strings.TrimSpace(ext.OrganizationExternalID) == "" {
		ext.OrganizationExternalID = existing.OrganizationExternalID
	}
	if strings.TrimSpace(ext.ExternalAccountID) == "" {
		ext.ExternalAccountID = existing.ExternalAccountID
	} else if strings.TrimSpace(existing.ExternalAccountID) != "" {
		ext.ExternalAccountID = existing.ExternalAccountID
	}
	if strings.TrimSpace(ext.DisplayName) == "" || externalUserDisplayNameIsFallback(ext) {
		ext.DisplayName = existing.DisplayName
	}
	if strings.TrimSpace(ext.Username) == "" {
		ext.Username = existing.Username
	}
	return ext, !ext.Disabled && ext.AccessEnabled, nil
}

func (s *Service) localUserByUsername(ctx context.Context, username string) (*types.User, error) {
	username = normalizeAccountName(username)
	if username == "" {
		return nil, nil
	}
	var user types.User
	if err := s.db.WithContext(ctx).
		Where("deleted_at IS NULL AND username = ?", username).
		First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

func (s *Service) exchangeSSOToken(ctx context.Context, setting *SyncSetting, code, redirectURI string) (*ssoTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", strings.TrimSpace(code))
	endpoint := setting.BaseURL + "/idp/api/v3/oauth2/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(setting.LoginClientID+":"+setting.LoginClientSecret)))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("IAM token exchange failed: status=%d body=%s", resp.StatusCode, trimForLog(string(body)))
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil {
		tokenResp := &ssoTokenResponse{
			AccessToken: ssoAccessTokenFromResponse(parsed),
			IDToken:     ssoIDTokenFromResponse(parsed),
		}
		if tokenResp.AccessToken != "" || tokenResp.IDToken != "" {
			return tokenResp, nil
		}
		if message := ssoErrorMessage(parsed); message != "" {
			return nil, fmt.Errorf("IAM token exchange failed: %s", message)
		}
		return nil, fmt.Errorf("IAM token exchange response missing access token: shape=%s", ssoResponseShape(parsed))
	}
	token := strings.Trim(strings.TrimSpace(string(body)), `"`)
	if token == "" {
		return nil, fmt.Errorf("IAM token exchange returned empty access token")
	}
	return &ssoTokenResponse{AccessToken: token}, nil
}

func (s *Service) resolveSSOUserInfo(ctx context.Context, setting *SyncSetting, tokenResp *ssoTokenResponse) (map[string]any, error) {
	if tokenResp == nil {
		return nil, fmt.Errorf("IAM token exchange returned empty token response")
	}
	if strings.TrimSpace(tokenResp.AccessToken) != "" {
		claims, err := s.fetchSSOUserInfo(ctx, setting, tokenResp.AccessToken)
		if err == nil {
			return claims, nil
		}
		if strings.TrimSpace(tokenResp.IDToken) == "" {
			return nil, err
		}
		logger.Warnf(ctx, "[custom iam] userinfo failed, falling back to id_token claims: %v", err)
	}
	if strings.TrimSpace(tokenResp.IDToken) != "" {
		return decodeSSOIDTokenClaims(tokenResp.IDToken)
	}
	return nil, fmt.Errorf("IAM token exchange response missing access token")
}

func (s *Service) fetchSSOUserInfo(ctx context.Context, setting *SyncSetting, accessToken string) (map[string]any, error) {
	endpoint := setting.BaseURL + "/idp/api/v3/oauth2/userInfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("IAM userinfo failed: status=%d body=%s", resp.StatusCode, trimForLog(string(body)))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if message := ssoErrorMessage(parsed); message != "" {
		return nil, fmt.Errorf("IAM userinfo failed: %s", message)
	}
	if data, ok := parsed["data"].(map[string]any); ok {
		return data, nil
	}
	return parsed, nil
}

func (s *Service) syncOnce(ctx context.Context, setting *SyncSetting, scopes ...SyncScope) (*SyncResult, error) {
	if strings.TrimSpace(setting.BaseURL) == "" || strings.TrimSpace(setting.SyncClientID) == "" || strings.TrimSpace(setting.SyncClientSecret) == "" {
		return nil, fmt.Errorf("IAM sync base_url, client_id and client_secret are required")
	}
	scope := SyncScope{}
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	token, err := s.login(ctx, setting)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := s.logout(context.Background(), setting, token); err != nil {
			logger.Warnf(context.Background(), "[custom iam] logout failed: %v", err)
		}
	}()

	result := &SyncResult{}
	if strings.TrimSpace(scope.OrganizationExternalID) != "" {
		return s.syncScopedOrganization(ctx, setting, token, scope, result)
	}
	orgCount, err := s.fetchPagedEach(ctx, setting, token, "/bim-server/ext/rest/integration/ExtApiIngtTargetOrganizationService/findBy", func(items []map[string]any) error {
		return s.upsertOrganizations(ctx, items)
	})
	if err != nil {
		return nil, err
	}
	result.OrgCount = orgCount

	userCount, err := s.fetchPagedEach(ctx, setting, token, "/bim-server/ext/rest/integration/ExtApiIngtTargetAccountService/findBy", func(items []map[string]any) error {
		return s.upsertUsers(ctx, items, result)
	})
	if err != nil {
		return nil, err
	}
	result.UserCount = userCount
	return result, nil
}

func (s *Service) syncScopedOrganization(ctx context.Context, setting *SyncSetting, token string, scope SyncScope, result *SyncResult) (*SyncResult, error) {
	rootID := strings.TrimSpace(scope.OrganizationExternalID)
	if rootID == "" {
		return result, nil
	}
	orgItems, orgScope, err := s.fetchOrganizationSubtree(ctx, setting, token, rootID)
	if err != nil {
		return nil, err
	}
	if len(orgScope) == 0 {
		return nil, fmt.Errorf("IAM organization %s not found in sync response", rootID)
	}
	result.OrgCount = len(orgItems)
	if err := s.upsertOrganizations(ctx, orgItems); err != nil {
		return nil, err
	}

	orgIDs := sortedScopeIDs(orgScope)
	for _, batch := range chunkStrings(orgIDs, iamFilterInBatchSize) {
		_, err = s.fetchPagedEach(ctx, setting, token, "/bim-server/ext/rest/integration/ExtApiIngtTargetAccountService/findBy", func(items []map[string]any) error {
			scopedUsers := filterUserItemsByOrganizationScope(items, orgScope)
			if len(scopedUsers) == 0 {
				return nil
			}
			result.UserCount += len(scopedUsers)
			return s.upsertUsers(ctx, scopedUsers, result)
		}, map[string]any{
			"organizationId_in": batch,
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

const iamFilterInBatchSize = 100

func (s *Service) fetchOrganizationSubtree(ctx context.Context, setting *SyncSetting, token, rootID string) ([]map[string]any, map[string]bool, error) {
	const orgPath = "/bim-server/ext/rest/integration/ExtApiIngtTargetOrganizationService/findBy"
	rootItems, err := s.fetchPaged(ctx, setting, token, orgPath, map[string]any{
		"organizationId_eq": rootID,
	})
	if err != nil {
		return nil, nil, err
	}
	if len(rootItems) == 0 {
		rootItems, err = s.fetchPaged(ctx, setting, token, orgPath, map[string]any{
			"_ID_eq": rootID,
		})
		if err != nil {
			return nil, nil, err
		}
	}

	orgItems := make([]map[string]any, 0, len(rootItems))
	scope := map[string]bool{}
	queue := make([]string, 0, len(rootItems))
	for _, item := range rootItems {
		orgID := syncOrganizationExternalID(item)
		if orgID == "" || scope[orgID] {
			continue
		}
		scope[orgID] = true
		queue = append(queue, orgID)
		orgItems = append(orgItems, item)
	}
	if len(scope) == 0 {
		return nil, nil, nil
	}

	for len(queue) > 0 {
		batchSize := iamFilterInBatchSize
		if batchSize > len(queue) {
			batchSize = len(queue)
		}
		parentBatch := append([]string(nil), queue[:batchSize]...)
		queue = queue[batchSize:]
		children, err := s.fetchPaged(ctx, setting, token, orgPath, map[string]any{
			"parentId_in": parentBatch,
		})
		if err != nil {
			return nil, nil, err
		}
		for _, child := range children {
			orgID := syncOrganizationExternalID(child)
			if orgID == "" || scope[orgID] {
				continue
			}
			scope[orgID] = true
			queue = append(queue, orgID)
			orgItems = append(orgItems, child)
		}
	}
	return orgItems, scope, nil
}

func buildOrganizationItemScope(items []map[string]any, rootID string) map[string]bool {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" {
		return nil
	}
	childrenByParent := make(map[string][]string, len(items))
	foundRoot := false
	for _, item := range items {
		orgID := syncOrganizationExternalID(item)
		if orgID == "" {
			continue
		}
		if orgID == rootID {
			foundRoot = true
		}
		parentID := strings.TrimSpace(firstString(item, "parentId", "parentOrganizationId"))
		childrenByParent[parentID] = append(childrenByParent[parentID], orgID)
	}
	if !foundRoot {
		return nil
	}
	scope := map[string]bool{}
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || scope[id] {
			continue
		}
		scope[id] = true
		queue = append(queue, childrenByParent[id]...)
	}
	return scope
}

func filterOrganizationItemsByScope(items []map[string]any, scope map[string]bool) []map[string]any {
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if scope[syncOrganizationExternalID(item)] {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func filterUserItemsByOrganizationScope(items []map[string]any, scope map[string]bool) []map[string]any {
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if scope[strings.TrimSpace(firstString(item, "organizationId"))] {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func sortedScopeIDs(scope map[string]bool) []string {
	ids := make([]string, 0, len(scope))
	for id := range scope {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func chunkStrings(values []string, size int) [][]string {
	if size <= 0 || len(values) == 0 {
		return nil
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

func syncOrganizationExternalID(item map[string]any) string {
	return strings.TrimSpace(firstString(item, "_ID", "id", "organizationId"))
}

func (s *Service) login(ctx context.Context, setting *SyncSetting) (string, error) {
	endpoint := setting.BaseURL + "/bim-server/ext/rest/integration/ExtApiIngtAuthService/login?force=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("clientId", setting.SyncClientID)
	req.Header.Set("clientSecret", setting.SyncClientSecret)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("IAM sync login failed: status=%d body=%s", resp.StatusCode, trimForLog(string(body)))
	}
	token := strings.Trim(strings.TrimSpace(string(body)), `"`)
	if token == "" {
		return "", fmt.Errorf("IAM sync login returned empty token")
	}
	return token, nil
}

func (s *Service) logout(ctx context.Context, setting *SyncSetting, token string) error {
	endpoint := setting.BaseURL + "/bim-server/ext/rest/integration/ExtApiIngtAuthService/logout"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("token", token)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("clientId", setting.SyncClientID)
	req.Header.Set("clientSecret", setting.SyncClientSecret)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (s *Service) fetchPaged(ctx context.Context, setting *SyncSetting, token, path string, filters ...map[string]any) ([]map[string]any, error) {
	var all []map[string]any
	_, err := s.fetchPagedEach(ctx, setting, token, path, func(items []map[string]any) error {
		all = append(all, items...)
		return nil
	}, filters...)
	return all, err
}

func (s *Service) fetchPagedEach(ctx context.Context, setting *SyncSetting, token, path string, handle func([]map[string]any) error, filters ...map[string]any) (int, error) {
	const pageSize = 200
	total := 0
	for page := 0; page < 1000; page++ {
		body := map[string]any{
			"number": page,
			"size":   pageSize,
		}
		if len(filters) > 0 && len(filters[0]) > 0 {
			body["filters"] = filters[0]
		}
		raw, _ := json.Marshal(body)
		endpoint := setting.BaseURL + path
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return total, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("token", token)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return total, err
		}
		responseBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return total, fmt.Errorf("IAM sync request failed: path=%s status=%d body=%s", path, resp.StatusCode, trimForLog(string(responseBody)))
		}
		items, hasMore, err := extractItems(responseBody, pageSize)
		if err != nil {
			return total, fmt.Errorf("parse IAM sync response failed: %w", err)
		}
		if len(items) > 0 {
			if handle != nil {
				if err := handle(items); err != nil {
					return total, err
				}
			}
			total += len(items)
		}
		if !hasMore || len(items) == 0 {
			break
		}
	}
	return total, nil
}

func (s *Service) upsertOrganizations(ctx context.Context, items []map[string]any) error {
	rows := make([]ExternalOrganization, 0, len(items))
	for _, item := range items {
		raw, _ := json.Marshal(item)
		externalID := firstString(item, "_ID", "id", "organizationId")
		if externalID == "" {
			continue
		}
		org := ExternalOrganization{
			ExternalID:         externalID,
			ExternalBusinessID: firstString(item, "_BID", "bid"),
			Code:               firstString(item, "code"),
			Name:               firstString(item, "name", "organizationName"),
			ParentExternalID:   firstString(item, "parentId", "parentOrganizationId"),
			Disabled:           firstBool(item, "isDisabled", "disabled"),
			Sequence:           firstString(item, "sequence"),
			ExternalUpdatedAt:  firstString(item, "updateAt", "updatedAt"),
			Raw:                string(raw),
		}
		if org.Name == "" {
			org.Name = externalID
		}
		rows = append(rows, org)
	}
	if len(rows) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "external_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"external_business_id", "code", "name", "parent_external_id", "disabled", "sequence", "external_updated_at", "raw", "updated_at",
		}),
	}).CreateInBatches(rows, 500).Error
}

func (s *Service) upsertUsers(ctx context.Context, items []map[string]any, result *SyncResult) error {
	rows := make([]ExternalUser, 0, len(items))
	externalIDs := make([]string, 0, len(items))
	lookupExternalIDs := make([]string, 0, len(items)*2)
	externalAccountIDs := make([]string, 0, len(items))
	seenIDs := map[string]bool{}
	seenLookupExternalIDs := map[string]bool{}
	seenAccountIDs := map[string]bool{}
	for _, item := range items {
		raw := marshalIAMUserRaw(item)
		externalAccountID := iamStableUserIDFromItem(item)
		username := iamAccountNameFromItem(item)
		externalID := externalAccountID
		if externalID == "" {
			externalID = username
		}
		if externalID == "" {
			continue
		}
		if username == "" {
			username = externalID
		}
		if seenIDs[externalID] {
			continue
		}
		seenIDs[externalID] = true
		externalIDs = append(externalIDs, externalID)
		for _, lookupID := range []string{externalID, username} {
			lookupID = strings.TrimSpace(lookupID)
			if lookupID != "" && !seenLookupExternalIDs[lookupID] {
				seenLookupExternalIDs[lookupID] = true
				lookupExternalIDs = append(lookupExternalIDs, lookupID)
			}
		}
		if externalAccountID != "" && !seenAccountIDs[externalAccountID] {
			seenAccountIDs[externalAccountID] = true
			externalAccountIDs = append(externalAccountIDs, externalAccountID)
		}
		displayName := firstString(item, "fullname", "fullName", "name", "displayName")
		if displayName == "" {
			displayName = username
		}

		ext := ExternalUser{
			ExternalID:             externalID,
			ExternalAccountID:      externalAccountID,
			Username:               username,
			DisplayName:            displayName,
			OrganizationExternalID: firstString(item, "organizationId"),
			Disabled:               firstBool(item, "isDisabled", "disabled"),
			ExternalUpdatedAt:      firstString(item, "updateAt", "updatedAt"),
			Raw:                    raw,
		}
		rows = append(rows, ext)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := s.attachLocalUsersByUsername(ctx, rows); err != nil {
		return err
	}

	existingByExternalID := make(map[string]ExternalUser, len(externalIDs))
	existingByExternalAccountID := make(map[string]ExternalUser, len(externalAccountIDs))
	for start := 0; start < len(lookupExternalIDs); start += 500 {
		end := start + 500
		if end > len(lookupExternalIDs) {
			end = len(lookupExternalIDs)
		}
		var existing []ExternalUser
		if err := s.db.WithContext(ctx).
			Where("(external_id IN ? OR username IN ?) AND deleted_at IS NULL", lookupExternalIDs[start:end], lookupExternalIDs[start:end]).
			Find(&existing).Error; err != nil {
			return err
		}
		for _, ext := range existing {
			existingByExternalID[ext.ExternalID] = ext
			if ext.ExternalAccountID != "" {
				existingByExternalAccountID[ext.ExternalAccountID] = ext
			}
		}
	}
	for start := 0; start < len(externalAccountIDs); start += 500 {
		end := start + 500
		if end > len(externalAccountIDs) {
			end = len(externalAccountIDs)
		}
		var existing []ExternalUser
		if err := s.db.WithContext(ctx).
			Where("external_account_id IN ?", externalAccountIDs[start:end]).
			Find(&existing).Error; err != nil {
			return err
		}
		for _, ext := range existing {
			existingByExternalID[ext.ExternalID] = ext
			if ext.ExternalAccountID != "" {
				existingByExternalAccountID[ext.ExternalAccountID] = ext
			}
		}
	}

	newRows := make([]ExternalUser, 0, len(rows))
	updatedRows := make([]ExternalUser, 0)
	for i := range rows {
		existing, ok := findExistingExternalUser(rows[i], existingByExternalID, existingByExternalAccountID)
		if ok {
			changed, err := s.updateExistingExternalUserIfChanged(ctx, existing, rows[i])
			if err != nil {
				return err
			}
			if changed {
				rows[i].ID = existing.ID
				if strings.TrimSpace(rows[i].WeKnoraUserID) == "" {
					rows[i].WeKnoraUserID = existing.WeKnoraUserID
					rows[i].AccessEnabled = existing.AccessEnabled
				}
				updatedRows = append(updatedRows, rows[i])
				result.UpdatedUsers++
				if rows[i].Disabled {
					result.DisabledUsers++
				}
			}
			continue
		}
		newRows = append(newRows, rows[i])
		result.CreatedUsers++
		if rows[i].Disabled {
			result.DisabledUsers++
		}
	}
	if len(updatedRows) > 0 {
		if err := s.updateLinkedLocalUsersForExternalRows(ctx, updatedRows); err != nil {
			return err
		}
	}

	if len(newRows) > 0 {
		return s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "external_id"}},
			DoNothing: true,
		}).CreateInBatches(newRows, 500).Error
	}
	return nil
}

func (s *Service) attachLocalUsersByUsername(ctx context.Context, rows []ExternalUser) error {
	usernames := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		username := strings.TrimSpace(row.Username)
		if username == "" || seen[username] {
			continue
		}
		seen[username] = true
		usernames = append(usernames, username)
	}
	if len(usernames) == 0 {
		return nil
	}

	var users []types.User
	if err := s.db.WithContext(ctx).
		Select("id", "username", "is_active").
		Where("deleted_at IS NULL AND username IN ?", usernames).
		Find(&users).Error; err != nil {
		return err
	}
	userByUsername := make(map[string]types.User, len(users))
	for _, user := range users {
		userByUsername[strings.TrimSpace(user.Username)] = user
	}
	for i := range rows {
		user, ok := userByUsername[strings.TrimSpace(rows[i].Username)]
		if !ok {
			continue
		}
		rows[i].WeKnoraUserID = user.ID
		rows[i].AccessEnabled = user.IsActive
	}
	return nil
}

func (s *Service) cleanupMirrorOnlyUsersWithLocalUsers(ctx context.Context) error {
	return s.db.WithContext(ctx).
		Unscoped().
		Where("COALESCE(weknora_user_id, '') = ''").
		Where(`EXISTS (
			SELECT 1 FROM users
			WHERE users.deleted_at IS NULL
			  AND users.username = custom_iam_users.username
		)`).
		Delete(&ExternalUser{}).Error
}

func findExistingExternalUser(incoming ExternalUser, byExternalID map[string]ExternalUser, byExternalAccountID map[string]ExternalUser) (ExternalUser, bool) {
	if incoming.ExternalAccountID != "" {
		if existing, ok := byExternalAccountID[incoming.ExternalAccountID]; ok {
			return existing, true
		}
	}
	if incoming.ExternalID != "" {
		if existing, ok := byExternalID[incoming.ExternalID]; ok {
			return existing, true
		}
	}
	if incoming.Username != "" {
		if existing, ok := byExternalID[incoming.Username]; ok {
			return existing, true
		}
	}
	return ExternalUser{}, false
}

func (s *Service) updateExistingExternalUserIfChanged(ctx context.Context, existing ExternalUser, incoming ExternalUser) (bool, error) {
	updates := map[string]any{}
	if strings.TrimSpace(existing.ExternalID) != strings.TrimSpace(incoming.ExternalID) {
		updates["external_id"] = incoming.ExternalID
	}
	if strings.TrimSpace(existing.ExternalAccountID) != strings.TrimSpace(incoming.ExternalAccountID) {
		updates["external_account_id"] = incoming.ExternalAccountID
	}
	if strings.TrimSpace(existing.Username) != strings.TrimSpace(incoming.Username) {
		updates["username"] = incoming.Username
	}
	if strings.TrimSpace(existing.DisplayName) != strings.TrimSpace(incoming.DisplayName) {
		updates["display_name"] = incoming.DisplayName
	}
	if strings.TrimSpace(existing.OrganizationExternalID) != strings.TrimSpace(incoming.OrganizationExternalID) {
		updates["organization_external_id"] = incoming.OrganizationExternalID
	}
	if existing.Disabled != incoming.Disabled {
		updates["disabled"] = incoming.Disabled
	}
	if strings.TrimSpace(incoming.WeKnoraUserID) != "" && strings.TrimSpace(existing.WeKnoraUserID) != strings.TrimSpace(incoming.WeKnoraUserID) {
		updates["weknora_user_id"] = incoming.WeKnoraUserID
		updates["access_enabled"] = incoming.AccessEnabled
	}
	if len(updates) == 0 {
		return false, nil
	}
	if oldExternalID, ok := updates["external_id"]; ok {
		if err := s.rewritePendingGrantExternalUserID(ctx, strings.TrimSpace(existing.ExternalID), strings.TrimSpace(oldExternalID.(string))); err != nil {
			return false, err
		}
	}
	updates["external_updated_at"] = incoming.ExternalUpdatedAt
	updates["raw"] = incoming.Raw
	updates["updated_at"] = time.Now()
	if err := s.db.WithContext(ctx).Model(&ExternalUser{}).
		Where("id = ?", existing.ID).
		Updates(updates).Error; err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) rewritePendingGrantExternalUserID(ctx context.Context, oldExternalID, newExternalID string) error {
	if oldExternalID == "" || newExternalID == "" || oldExternalID == newExternalID {
		return nil
	}
	return s.db.WithContext(ctx).
		Table("custom_iam_pending_space_member_grants").
		Where("iam_external_user_id = ?", oldExternalID).
		Update("iam_external_user_id", newExternalID).Error
}

func (s *Service) updateLinkedLocalUsersForExternalRows(ctx context.Context, rows []ExternalUser) error {
	for _, row := range rows {
		userID := strings.TrimSpace(row.WeKnoraUserID)
		if userID == "" {
			continue
		}
		if err := s.updateLocalUser(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) CreatePendingSpaceMemberGrant(ctx context.Context, input PendingSpaceMemberGrantInput) error {
	orgID := strings.TrimSpace(input.OrganizationID)
	externalUserID := strings.TrimSpace(input.IAMExternalUserID)
	if orgID == "" {
		return fmt.Errorf("organization_id is required")
	}
	if externalUserID == "" {
		return fmt.Errorf("iam_external_user_id is required")
	}
	if !input.Role.IsValid() {
		return fmt.Errorf("invalid role")
	}

	var ext ExternalUser
	if err := s.db.WithContext(ctx).
		Where("external_id = ? AND deleted_at IS NULL AND disabled = ?", externalUserID, false).
		First(&ext).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrIAMExternalUserNotFound
		}
		return err
	}

	if err := s.ensurePendingGrantCapacity(ctx, orgID); err != nil {
		return err
	}

	grant := PendingSpaceMemberGrant{
		OrganizationID:    orgID,
		IAMExternalUserID: externalUserID,
		Role:              input.Role,
		InvitedByUserID:   strings.TrimSpace(input.InvitedByUserID),
	}
	if err := s.db.WithContext(ctx).Create(&grant).Error; err != nil {
		if isUniqueConstraintError(err) {
			return ErrPendingSpaceMemberGrantExists
		}
		return err
	}
	return nil
}

func (s *Service) ensurePendingGrantCapacity(ctx context.Context, orgID string) error {
	var org types.Organization
	if err := s.db.WithContext(ctx).Select("id", "member_limit").First(&org, "id = ?", orgID).Error; err != nil {
		return err
	}
	if org.MemberLimit <= 0 {
		return nil
	}
	var memberCount int64
	if err := s.db.WithContext(ctx).
		Model(&types.OrganizationTenantMember{}).
		Where("organization_id = ?", orgID).
		Count(&memberCount).Error; err != nil {
		return err
	}
	var pendingCount int64
	if err := s.db.WithContext(ctx).
		Model(&PendingSpaceMemberGrant{}).
		Where("organization_id = ? AND redeemed_at IS NULL", orgID).
		Count(&pendingCount).Error; err != nil {
		return err
	}
	if memberCount+pendingCount >= int64(org.MemberLimit) {
		return ErrPendingSpaceMemberLimitFull
	}
	return nil
}

func (s *Service) RedeemPendingSpaceMemberGrants(ctx context.Context, iamExternalUserID string, user *types.User) error {
	iamExternalUserID = strings.TrimSpace(iamExternalUserID)
	if iamExternalUserID == "" || user == nil || user.TenantID == 0 {
		return nil
	}

	var grants []PendingSpaceMemberGrant
	if err := s.db.WithContext(ctx).
		Where("iam_external_user_id = ? AND redeemed_at IS NULL", iamExternalUserID).
		Order("created_at ASC").
		Find(&grants).Error; err != nil {
		return err
	}
	for _, grant := range grants {
		if err := s.redeemPendingSpaceMemberGrant(ctx, &grant, user); err != nil {
			logger.Warnf(ctx, "[custom iam] failed to redeem pending space member grant %s for user %s: %v", grant.ID, user.ID, err)
		}
	}
	return nil
}

func (s *Service) redeemPendingSpaceMemberGrant(ctx context.Context, grant *PendingSpaceMemberGrant, user *types.User) error {
	if grant == nil || user == nil || user.TenantID == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current PendingSpaceMemberGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&current, "id = ? AND redeemed_at IS NULL", grant.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		var existing types.OrganizationTenantMember
		err := tx.Where("organization_id = ? AND tenant_id = ?", current.OrganizationID, user.TenantID).First(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			var org types.Organization
			if err := tx.Select("id", "member_limit").First(&org, "id = ?", current.OrganizationID).Error; err != nil {
				return err
			}
			if org.MemberLimit > 0 {
				var count int64
				if err := tx.Model(&types.OrganizationTenantMember{}).Where("organization_id = ?", current.OrganizationID).Count(&count).Error; err != nil {
					return err
				}
				if count >= int64(org.MemberLimit) {
					return ErrPendingSpaceMemberLimitFull
				}
			}
			now := time.Now()
			member := types.OrganizationTenantMember{
				ID:                   uuid.New().String(),
				OrganizationID:       current.OrganizationID,
				TenantID:             user.TenantID,
				Role:                 current.Role,
				RepresentativeUserID: user.ID,
				JoinedAt:             &now,
				CreatedAt:            now,
				UpdatedAt:            now,
			}
			if err := tx.Create(&member).Error; err != nil {
				if !isUniqueConstraintError(err) {
					return err
				}
			}
		}

		now := time.Now()
		return tx.Model(&PendingSpaceMemberGrant{}).Where("id = ?", current.ID).Updates(map[string]any{
			"redeemed_at":        &now,
			"redeemed_tenant_id": user.TenantID,
			"redeemed_user_id":   user.ID,
		}).Error
	})
}

func (s *Service) ensureLocalUser(ctx context.Context, ext ExternalUser) (*types.User, error) {
	accountName := normalizeAccountName(ext.Username)
	if accountName == "" {
		return nil, fmt.Errorf("IAM user missing username")
	}
	if user, err := s.userService.GetUserByUsername(ctx, accountName); err == nil && user != nil {
		if err := s.updateLocalUser(ctx, withLocalUserID(ext, user.ID)); err != nil {
			return nil, err
		}
		user.DisplayName = strings.TrimSpace(ext.DisplayName)
		user.IsActive = !ext.Disabled && ext.AccessEnabled
		// Provisioner is best-effort: defaults that fail must not block login.
		s.logProvisionErr(ctx, user, s.runProvisioner(ctx, user))
		return user, nil
	}
	password, err := appservice.GenerateCompliantRandomPassword()
	if err != nil {
		return nil, err
	}
	user, err := s.userService.Register(ctx, &types.RegisterRequest{
		Username:    accountName,
		DisplayName: strings.TrimSpace(ext.DisplayName),
		Password:    password,
	})
	if err != nil {
		return nil, err
	}
	ext.WeKnoraUserID = user.ID
	if err := s.updateLocalUser(ctx, ext); err != nil {
		return nil, err
	}
	user.DisplayName = strings.TrimSpace(ext.DisplayName)
	user.IsActive = !ext.Disabled && ext.AccessEnabled
	// Provisioner is best-effort: defaults that fail must not block login.
	s.logProvisionErr(ctx, user, s.runProvisioner(ctx, user))
	return user, nil
}

func (s *Service) RunProvisioner(ctx context.Context, user *types.User) error {
	if s.provisionUser == nil || user == nil {
		return nil
	}
	if err := s.provisionUser(ctx, user); err != nil {
		return fmt.Errorf("provision IAM user defaults: %w", err)
	}
	return nil
}

func (s *Service) logProvisionErr(ctx context.Context, user *types.User, err error) {
	if err != nil && user != nil {
		logger.Warnf(ctx, "[iam] provisioner failed for user %s (non-fatal): %v", user.Username, err)
	}
}

func (s *Service) runProvisioner(ctx context.Context, user *types.User) error {
	return s.RunProvisioner(ctx, user)
}

func (s *Service) updateLocalUser(ctx context.Context, ext ExternalUser) error {
	if ext.WeKnoraUserID == "" {
		return nil
	}
	var user types.User
	if err := s.db.WithContext(ctx).First(&user, "id = ?", ext.WeKnoraUserID).Error; err != nil {
		return err
	}
	updates := map[string]any{
		"is_active":  !ext.Disabled && ext.AccessEnabled,
		"updated_at": time.Now(),
	}
	if accountName := normalizeAccountName(ext.Username); accountName != "" {
		updates["username"] = accountName
	}
	if displayName := strings.TrimSpace(ext.DisplayName); displayName != "" {
		updates["display_name"] = displayName
	}
	return s.db.WithContext(ctx).Model(&types.User{}).Where("id = ?", ext.WeKnoraUserID).Updates(updates).Error
}

func (s *Service) StartScheduler(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scheduler != nil {
		return nil
	}
	location := time.Local
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		location = loc
	}
	s.scheduler = cron.New(cron.WithLocation(location))
	s.scheduler.Start()
	return s.reloadScheduleLocked(ctx)
}

func (s *Service) ReloadSchedule(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scheduler == nil {
		return nil
	}
	return s.reloadScheduleLocked(ctx)
}

func (s *Service) reloadScheduleLocked(ctx context.Context) error {
	if s.entryID != 0 {
		s.scheduler.Remove(s.entryID)
		s.entryID = 0
	}
	setting, err := s.GetSetting(ctx)
	if err != nil {
		return err
	}
	if !setting.Enabled {
		return nil
	}
	spec, err := cronSpec(setting)
	if err != nil {
		return err
	}
	entryID, err := s.scheduler.AddFunc(spec, func() {
		runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := s.RunSync(runCtx, "schedule"); err != nil {
			logger.Errorf(runCtx, "[custom iam] scheduled sync failed: %v", err)
		}
	})
	if err != nil {
		return err
	}
	s.entryID = entryID
	return nil
}

func cronSpec(setting *SyncSetting) (string, error) {
	hour, minute, err := parseRunAt(setting.RunAt)
	if err != nil {
		return "", err
	}
	if setting.ScheduleMode == ScheduleModeWeekly {
		days := normalizeWeekdays(setting.Weekdays)
		if days == "" {
			days = "1"
		}
		return fmt.Sprintf("%d %d * * %s", minute, hour, days), nil
	}
	return fmt.Sprintf("%d %d * * *", minute, hour), nil
}

func normalizeSetting(setting *SyncSetting) {
	if setting.ScheduleMode != ScheduleModeWeekly {
		setting.ScheduleMode = ScheduleModeDaily
	}
	setting.Weekdays = normalizeWeekdays(setting.Weekdays)
	if _, _, err := parseRunAt(setting.RunAt); err != nil {
		setting.RunAt = DefaultRunAt
	}
	setting.BaseURL = strings.TrimRight(strings.TrimSpace(setting.BaseURL), "/")
}

func parseRunAt(value string) (hour int, minute int, err error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("run_at must be HH:mm")
	}
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil {
		return 0, 0, fmt.Errorf("run_at must be HH:mm")
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("run_at must be HH:mm")
	}
	return hour, minute, nil
}

func normalizeWeekdays(value string) string {
	seen := map[string]bool{}
	var days []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		if part >= "0" && part <= "6" {
			seen[part] = true
			days = append(days, part)
		}
	}
	return strings.Join(days, ",")
}

func extractItems(raw []byte, pageSize int) ([]map[string]any, bool, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, err
	}
	items := findArray(value)
	if len(items) == 0 {
		if item, ok := value.(map[string]any); ok && looksLikeIAMItem(item) {
			items = []map[string]any{item}
		}
	}
	hasMore := len(items) >= pageSize
	return items, hasMore, nil
}

func findArray(value any) []map[string]any {
	switch v := value.(type) {
	case []any:
		return anySliceToMaps(v)
	case map[string]any:
		for _, key := range []string{"content", "records", "rows", "list", "data", "result"} {
			if child, ok := v[key]; ok {
				if items := findArray(child); len(items) > 0 {
					return items
				}
			}
		}
	}
	return nil
}

func anySliceToMaps(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func looksLikeIAMItem(item map[string]any) bool {
	for _, key := range []string{"username", "userName", "userId", "organizationId", "_ID", "_AID", "_BID", "name"} {
		if value := firstString(item, key); value != "" {
			return true
		}
	}
	return false
}

func firstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, ok := item[key]; ok && raw != nil {
			switch v := raw.(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			case float64:
				return fmt.Sprintf("%.0f", v)
			default:
				return strings.TrimSpace(fmt.Sprintf("%v", v))
			}
		}
	}
	return ""
}

func firstBool(item map[string]any, keys ...string) bool {
	for _, key := range keys {
		if raw, ok := item[key]; ok && raw != nil {
			switch v := raw.(type) {
			case bool:
				return v
			case string:
				return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
			case float64:
				return v != 0
			}
		}
	}
	return false
}

func ssoAccessTokenFromResponse(value any) string {
	return ssoTokenFromResponse(value, true, isSSOAccessTokenKey)
}

func ssoIDTokenFromResponse(value any) string {
	return ssoTokenFromResponse(value, false, isSSOIDTokenKey)
}

func ssoTokenFromResponse(value any, allowBareString bool, matchKey func(string) bool) string {
	switch v := value.(type) {
	case string:
		if allowBareString {
			return strings.TrimSpace(v)
		}
	case map[string]any:
		for key, child := range v {
			if matchKey(key) {
				if token := ssoStringValue(child); token != "" {
					return token
				}
				if token := ssoTokenFromResponse(child, true, matchKey); token != "" {
					return token
				}
			}
		}
		for _, key := range []string{"data", "datas", "result", "resultData", "payload", "body", "tokenInfo", "token_info", "oauth", "oidc"} {
			if child, ok := v[key]; ok {
				if token := ssoTokenFromResponse(child, true, matchKey); token != "" {
					return token
				}
			}
		}
		for _, child := range v {
			if token := ssoTokenFromResponse(child, false, matchKey); token != "" {
				return token
			}
		}
	case []any:
		for _, item := range v {
			if token := ssoTokenFromResponse(item, false, matchKey); token != "" {
				return token
			}
		}
	}
	return ""
}

func ssoErrorMessage(value any) string {
	item, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	errCode := firstString(item, "errcode", "errCode", "error", "code", "status")
	if errCode == "" || errCode == "0" || errCode == "200" || strings.EqualFold(errCode, "ok") || strings.EqualFold(errCode, "success") {
		return ""
	}
	message := firstString(item, "msg", "error_description", "message")
	if message == "" {
		message = errCode
	}
	return message
}

func isSSOAccessTokenKey(key string) bool {
	switch normalizeSSOKey(key) {
	case "accesstoken", "accesstokenvalue", "token", "tokenvalue", "oauthtoken", "oidctoken":
		return true
	default:
		return false
	}
}

func isSSOIDTokenKey(key string) bool {
	switch normalizeSSOKey(key) {
	case "idtoken", "idtokenvalue", "jwt", "jwttoken":
		return true
	default:
		return false
	}
}

func normalizeSSOKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	replacer := strings.NewReplacer("_", "", "-", "", ".", "")
	return replacer.Replace(key)
}

func ssoStringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

func ssoResponseShape(value any) string {
	raw, err := json.Marshal(ssoShapeValue(value, 0))
	if err != nil {
		return "unavailable"
	}
	return string(raw)
}

func decodeSSOIDTokenClaims(idToken string) (map[string]any, error) {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("IAM id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if padded := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4); padded != parts[1] {
			payload, err = base64.URLEncoding.DecodeString(padded)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("decode IAM id_token payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse IAM id_token claims: %w", err)
	}
	return claims, nil
}

func ssoShapeValue(value any, depth int) any {
	if depth >= 3 {
		return "..."
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			out[key] = ssoShapeValue(child, depth+1)
		}
		return out
	case []any:
		if len(v) == 0 {
			return "array[0]"
		}
		return []any{fmt.Sprintf("array[%d]", len(v)), ssoShapeValue(v[0], depth+1)}
	case string:
		return fmt.Sprintf("string[%d]", len(v))
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func safeLocalPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = uuid.NewString()
	}
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "@", "_")
	re := regexp.MustCompile(`[^a-z0-9_\-.]+`)
	value = re.ReplaceAllString(value, "_")
	value = strings.Trim(value, "._-")
	if value == "" {
		value = uuid.NewString()[:8]
	}
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

func withLocalUserID(ext ExternalUser, userID string) ExternalUser {
	ext.WeKnoraUserID = userID
	return ext
}

func externalUserFromClaims(claims map[string]any) ExternalUser {
	username := iamAccountNameFromClaims(claims)
	externalID := iamStableUserIDFromClaims(claims)
	if externalID == "" {
		externalID = username
	}
	displayName := firstString(claims, "fullname", "fullName", "name", "displayName")
	if username == "" {
		externalID = firstString(claims, "sub")
		username = externalID
	}
	if displayName == "" {
		displayName = username
	}
	raw, _ := json.Marshal(claims)
	return ExternalUser{
		ExternalID:        externalID,
		ExternalAccountID: firstString(claims, "uid", "userId", "user_id", "sub"),
		Username:          username,
		DisplayName:       displayName,
		Raw:               string(raw),
	}
}

func iamAccountNameFromItem(item map[string]any) string {
	return normalizeAccountName(firstString(item, "username", "userName", "loginName", "account", "userId", "_ID", "id"))
}

func iamStableUserIDFromItem(item map[string]any) string {
	return normalizeAccountName(firstString(item, "_AID", "accountId", "id", "_ID", "userId"))
}

func iamAccountNameFromClaims(claims map[string]any) string {
	return normalizeAccountName(firstString(claims, "userName", "username", "loginName", "account", "uid", "userId", "user_id", "sub"))
}

func iamStableUserIDFromClaims(claims map[string]any) string {
	return normalizeAccountName(firstString(claims, "_AID", "accountId", "id", "uid", "userId", "user_id", "sub"))
}

func externalUserDisplayNameIsFallback(ext ExternalUser) bool {
	displayName := strings.TrimSpace(ext.DisplayName)
	if displayName == "" {
		return true
	}
	for _, fallback := range []string{ext.Username, ext.ExternalID, ext.ExternalAccountID} {
		fallback = strings.TrimSpace(fallback)
		if fallback != "" && strings.EqualFold(displayName, fallback) {
			return true
		}
	}
	return false
}

func marshalIAMUserRaw(item map[string]any) string {
	if len(item) == 0 {
		return "{}"
	}
	sanitized := make(map[string]any, len(item))
	for key, value := range item {
		if strings.EqualFold(strings.TrimSpace(key), "password") {
			continue
		}
		sanitized[key] = value
	}
	raw, err := json.Marshal(sanitized)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func normalizeAccountName(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 100 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= 100 {
		return value
	}
	return string(runes[:100])
}

func encodeSSOState(payload ssoStatePayload) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeSSOState(rawState string) (*ssoStatePayload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(rawState))
	if err != nil {
		return nil, err
	}
	var payload ssoStatePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if strings.TrimSpace(payload.RedirectURI) == "" {
		return nil, fmt.Errorf("state.redirect_uri is required")
	}
	return &payload, nil
}

func EncodeCallbackPayload(resp *types.OIDCCallbackResponse) (string, error) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func trimForLog(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 300 {
		return value[:300] + "..."
	}
	return value
}

func MaskSetting(setting *SyncSetting) *SyncSetting {
	if setting == nil {
		return nil
	}
	copy := *setting
	if copy.LoginClientSecret != "" {
		copy.LoginClientSecret = "******"
	}
	if copy.SyncClientSecret != "" {
		copy.SyncClientSecret = "******"
	}
	return &copy
}

func shouldReplaceSecret(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "******"
}

func ValidateLoginBaseURL(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	_, err := url.ParseRequestURI(value)
	return err
}

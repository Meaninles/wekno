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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

type SyncResult struct {
	OrgCount      int `json:"org_count"`
	UserCount     int `json:"user_count"`
	CreatedUsers  int `json:"created_users"`
	UpdatedUsers  int `json:"updated_users"`
	DisabledUsers int `json:"disabled_users"`
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
	if err := s.db.WithContext(ctx).AutoMigrate(&SyncSetting{}, &ExternalOrganization{}, &ExternalUser{}, &SyncRun{}); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Exec("ALTER TABLE custom_iam_users DROP COLUMN IF EXISTS email").Error
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
	err := s.db.WithContext(ctx).Order("started_at DESC").Limit(limit).Find(&runs).Error
	return runs, err
}

func (s *Service) ListSpaceMemberCandidateOrganizations(ctx context.Context, spaceID, query string, limit int) ([]SpaceMemberCandidateOrganization, error) {
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return nil, fmt.Errorf("space_id is required")
	}
	limit = normalizeCandidateLimit(limit, 1000, 5000)

	type organizationRow struct {
		ExternalID       string
		Name             string
		Code             string
		ParentExternalID string
	}
	type directCountRow struct {
		OrganizationExternalID string
		UserCount              int64
		TenantCount            int64
	}

	var orgs []organizationRow
	if err := s.db.WithContext(ctx).
		Table("custom_iam_organizations AS io").
		Select("io.external_id, io.name, io.code, io.parent_external_id").
		Where("io.deleted_at IS NULL AND io.disabled = ?", false).
		Where("io.external_id <> ''").
		Order("io.name ASC, io.external_id ASC").
		Scan(&orgs).Error; err != nil {
		return nil, err
	}

	var directCounts []directCountRow
	if err := s.db.WithContext(ctx).
		Table("custom_iam_users AS iu").
		Select(`
			iu.organization_external_id,
			COUNT(DISTINCT iu.weknora_user_id) AS user_count,
			COUNT(DISTINCT u.tenant_id) AS tenant_count`).
		Joins("JOIN users AS u ON u.id = iu.weknora_user_id AND u.deleted_at IS NULL AND u.is_active = ? AND u.tenant_id > 0", true).
		Joins("JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
		Where("iu.deleted_at IS NULL AND iu.disabled = ?", false).
		Where("COALESCE(iu.weknora_user_id, '') <> '' AND COALESCE(iu.organization_external_id, '') <> ''").
		Group("iu.organization_external_id").
		Scan(&directCounts).Error; err != nil {
		return nil, err
	}

	directUserCountByOrg := make(map[string]int64, len(directCounts))
	directTenantCountByOrg := make(map[string]int64, len(directCounts))
	for _, count := range directCounts {
		orgID := strings.TrimSpace(count.OrganizationExternalID)
		if orgID == "" {
			continue
		}
		directUserCountByOrg[orgID] = count.UserCount
		directTenantCountByOrg[orgID] = count.TenantCount
	}

	childrenByParent := make(map[string][]string, len(orgs))
	for _, org := range orgs {
		orgID := strings.TrimSpace(org.ExternalID)
		if orgID == "" {
			continue
		}
		parentID := strings.TrimSpace(org.ParentExternalID)
		childrenByParent[parentID] = append(childrenByParent[parentID], orgID)
	}

	totalUserCountByOrg := make(map[string]int64, len(orgs))
	totalTenantCountByOrg := make(map[string]int64, len(orgs))
	visiting := make(map[string]bool, len(orgs))
	var aggregateCounts func(string) (int64, int64)
	aggregateCounts = func(orgID string) (int64, int64) {
		if userCount, ok := totalUserCountByOrg[orgID]; ok {
			return userCount, totalTenantCountByOrg[orgID]
		}
		if visiting[orgID] {
			return directUserCountByOrg[orgID], directTenantCountByOrg[orgID]
		}
		visiting[orgID] = true
		userCount := directUserCountByOrg[orgID]
		tenantCount := directTenantCountByOrg[orgID]
		for _, childID := range childrenByParent[orgID] {
			childUserCount, childTenantCount := aggregateCounts(childID)
			userCount += childUserCount
			tenantCount += childTenantCount
		}
		visiting[orgID] = false
		totalUserCountByOrg[orgID] = userCount
		totalTenantCountByOrg[orgID] = tenantCount
		return userCount, tenantCount
	}

	queryText := strings.ToLower(strings.TrimSpace(query))
	rows := make([]SpaceMemberCandidateOrganization, 0, len(orgs))
	for _, org := range orgs {
		if queryText != "" &&
			!strings.Contains(strings.ToLower(org.Name), queryText) &&
			!strings.Contains(strings.ToLower(org.Code), queryText) &&
			!strings.Contains(strings.ToLower(org.ExternalID), queryText) {
			continue
		}
		userCount, tenantCount := aggregateCounts(org.ExternalID)
		rows = append(rows, SpaceMemberCandidateOrganization{
			ExternalID:       org.ExternalID,
			Name:             org.Name,
			ParentExternalID: org.ParentExternalID,
			UserCount:        userCount,
			TenantCount:      tenantCount,
		})
		if len(rows) >= limit {
			break
		}
	}
	return rows, nil
}

func (s *Service) ListSpaceMemberCandidateUsers(ctx context.Context, spaceID, query string, iamOrgExternalIDs []string, limit int) ([]SpaceMemberCandidateUser, error) {
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

	db := s.db.WithContext(ctx).
		Table("custom_iam_users AS iu").
		Select(`
			iu.weknora_user_id AS user_id,
			u.username,
			iu.display_name,
			u.avatar,
			u.tenant_id,
			t.name AS tenant_name,
			iu.organization_external_id AS iam_organization_external_id,
			io.name AS iam_organization_name`).
		Joins("JOIN users AS u ON u.id = iu.weknora_user_id AND u.deleted_at IS NULL AND u.is_active = ?", true).
		Joins("JOIN tenants AS t ON t.id = u.tenant_id AND t.deleted_at IS NULL").
		Joins("LEFT JOIN custom_iam_organizations AS io ON io.external_id = iu.organization_external_id AND io.deleted_at IS NULL").
		Where("iu.deleted_at IS NULL AND iu.disabled = ?", false).
		Where("COALESCE(iu.weknora_user_id, '') <> '' AND u.tenant_id > 0")

	if len(existingTenantIDs) > 0 {
		db = db.Where("u.tenant_id NOT IN ?", existingTenantIDs)
	}
	orgScope, err := s.resolveIAMOrganizationScope(ctx, iamOrgExternalIDs)
	if err != nil {
		return nil, err
	}
	if len(orgScope) > 0 {
		db = db.Where("iu.organization_external_id IN ?", orgScope)
	}
	if pattern := candidateSearchPattern(query); pattern != "" {
		db = db.Where(`
			LOWER(COALESCE(iu.username, '')) LIKE ? OR
			LOWER(COALESCE(iu.display_name, '')) LIKE ? OR
			LOWER(COALESCE(u.username, '')) LIKE ? OR
			LOWER(COALESCE(t.name, '')) LIKE ? OR
			LOWER(COALESCE(io.name, '')) LIKE ?`,
			pattern, pattern, pattern, pattern, pattern)
	}

	var rows []SpaceMemberCandidateUser
	if err := db.
		Order("COALESCE(io.name, '') ASC, COALESCE(iu.display_name, '') ASC, u.username ASC").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Service) resolveIAMOrganizationScope(ctx context.Context, rootExternalIDs []string) ([]string, error) {
	roots := make([]string, 0, len(rootExternalIDs))
	seenRoots := map[string]bool{}
	for _, id := range rootExternalIDs {
		id = strings.TrimSpace(id)
		if id == "" || seenRoots[id] {
			continue
		}
		seenRoots[id] = true
		roots = append(roots, id)
	}
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

func (s *Service) RunSync(ctx context.Context, triggeredBy string) (*SyncRun, error) {
	if running, ok, err := s.currentRunningRun(ctx); err != nil {
		return nil, err
	} else if ok {
		return running, nil
	}

	run := &SyncRun{
		TriggeredBy: strings.TrimSpace(triggeredBy),
		Status:      "running",
		StartedAt:   time.Now(),
	}
	if run.TriggeredBy == "" {
		run.TriggeredBy = "manual"
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
	result, syncErr := s.syncOnce(ctx, setting)
	s.finishRun(ctx, &run, result, syncErr)
}

func (s *Service) finishRun(ctx context.Context, run *SyncRun, result *SyncResult, syncErr error) {
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
	if err := s.db.WithContext(ctx).Save(run).Error; err != nil {
		logger.Errorf(ctx, "[custom iam] failed to save sync run %s: %v", run.ID, err)
		return
	}
	if err := s.db.WithContext(ctx).Model(&SyncSetting{}).Where("id = ?", 1).Updates(map[string]any{
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
	user, err := s.ensureLocalUser(ctx, ext)
	if err != nil {
		return nil, err
	}
	ext.WeKnoraUserID = user.ID
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "external_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"username", "display_name", "weknora_user_id", "raw", "updated_at",
		}),
	}).Create(&ext).Error; err != nil {
		return nil, err
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

func (s *Service) syncOnce(ctx context.Context, setting *SyncSetting) (*SyncResult, error) {
	if strings.TrimSpace(setting.BaseURL) == "" || strings.TrimSpace(setting.SyncClientID) == "" || strings.TrimSpace(setting.SyncClientSecret) == "" {
		return nil, fmt.Errorf("IAM sync base_url, client_id and client_secret are required")
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

	orgItems, err := s.fetchPaged(ctx, setting, token, "/bim-server/ext/rest/integration/ExtApiIngtTargetOrganizationService/findBy")
	if err != nil {
		return nil, err
	}
	userItems, err := s.fetchPaged(ctx, setting, token, "/bim-server/ext/rest/integration/ExtApiIngtTargetAccountService/findBy")
	if err != nil {
		return nil, err
	}

	result := &SyncResult{OrgCount: len(orgItems), UserCount: len(userItems)}
	if err := s.upsertOrganizations(ctx, orgItems); err != nil {
		return nil, err
	}
	if err := s.upsertUsers(ctx, userItems, result); err != nil {
		return nil, err
	}
	return result, nil
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

func (s *Service) fetchPaged(ctx context.Context, setting *SyncSetting, token, path string) ([]map[string]any, error) {
	const pageSize = 200
	var all []map[string]any
	for page := 0; page < 1000; page++ {
		body := map[string]any{
			"number": page,
			"size":   pageSize,
		}
		raw, _ := json.Marshal(body)
		endpoint := setting.BaseURL + path
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("token", token)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		responseBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("IAM sync request failed: path=%s status=%d body=%s", path, resp.StatusCode, trimForLog(string(responseBody)))
		}
		items, hasMore, err := extractItems(responseBody, pageSize)
		if err != nil {
			return nil, fmt.Errorf("parse IAM sync response failed: %w", err)
		}
		all = append(all, items...)
		if !hasMore || len(items) == 0 {
			break
		}
	}
	return all, nil
}

func (s *Service) upsertOrganizations(ctx context.Context, items []map[string]any) error {
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
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "external_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"external_business_id", "code", "name", "parent_external_id", "disabled", "sequence", "external_updated_at", "raw", "updated_at",
			}),
		}).Create(&org).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) upsertUsers(ctx context.Context, items []map[string]any, result *SyncResult) error {
	for _, item := range items {
		raw, _ := json.Marshal(item)
		externalID := iamAccountNameFromItem(item)
		if externalID == "" {
			continue
		}
		username := externalID
		displayName := firstString(item, "fullname", "fullName", "name", "displayName")
		if displayName == "" {
			displayName = username
		}

		ext := ExternalUser{
			ExternalID:             externalID,
			ExternalAccountID:      firstString(item, "_AID", "accountId", "userId", "_ID"),
			Username:               username,
			DisplayName:            displayName,
			OrganizationExternalID: firstString(item, "organizationId"),
			Disabled:               firstBool(item, "isDisabled", "disabled"),
			ExternalUpdatedAt:      firstString(item, "updateAt", "updatedAt"),
			Raw:                    string(raw),
		}

		var existing ExternalUser
		err := s.db.WithContext(ctx).First(&existing, "external_id = ?", externalID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			localUser, err := s.ensureLocalUser(ctx, ext)
			if err != nil {
				return err
			}
			ext.WeKnoraUserID = localUser.ID
			result.CreatedUsers++
		} else {
			ext.ID = existing.ID
			ext.WeKnoraUserID = existing.WeKnoraUserID
			if ext.WeKnoraUserID == "" {
				localUser, err := s.ensureLocalUser(ctx, ext)
				if err != nil {
					return err
				}
				ext.WeKnoraUserID = localUser.ID
				result.CreatedUsers++
			} else if err := s.updateLocalUser(ctx, ext); err != nil {
				return err
			} else {
				result.UpdatedUsers++
			}
		}
		if ext.Disabled {
			result.DisabledUsers++
		}
		if err := s.db.WithContext(ctx).Save(&ext).Error; err != nil {
			return err
		}
	}
	return nil
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
		if err := s.runProvisioner(ctx, user); err != nil {
			return nil, err
		}
		return user, nil
	}
	user, err := s.userService.Register(ctx, &types.RegisterRequest{
		Username: accountName,
		Password: uuid.NewString() + uuid.NewString(),
	})
	if err != nil {
		return nil, err
	}
	ext.WeKnoraUserID = user.ID
	if err := s.updateLocalUser(ctx, ext); err != nil {
		return nil, err
	}
	if err := s.runProvisioner(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

func (s *Service) runProvisioner(ctx context.Context, user *types.User) error {
	if s.provisionUser == nil || user == nil {
		return nil
	}
	if err := s.provisionUser(ctx, user); err != nil {
		return fmt.Errorf("provision IAM user defaults: %w", err)
	}
	return nil
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
		"is_active":  !ext.Disabled,
		"updated_at": time.Now(),
	}
	if accountName := normalizeAccountName(ext.Username); accountName != "" {
		updates["username"] = accountName
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
	externalID := iamAccountNameFromClaims(claims)
	username := externalID
	displayName := firstString(claims, "fullname", "fullName", "name", "displayName", "userName")
	if externalID == "" {
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

func iamAccountNameFromClaims(claims map[string]any) string {
	return normalizeAccountName(firstString(claims, "userName", "username", "loginName", "account", "uid", "userId", "user_id", "sub"))
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

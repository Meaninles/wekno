package dbanalytics

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db              *gorm.DB
	duckdb          *sql.DB
	schemaReasoning sync.Map
}

type sourceShareMigration struct {
	ID             string              `gorm:"type:varchar(36);primaryKey"`
	SourceID       string              `gorm:"type:varchar(36);not null;index"`
	OrganizationID string              `gorm:"type:varchar(36);not null;index"`
	SharedByUserID string              `gorm:"type:varchar(36);not null"`
	SourceTenantID uint64              `gorm:"not null;index"`
	Permission     types.OrgMemberRole `gorm:"type:varchar(32);not null;default:'viewer'"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      gorm.DeletedAt `gorm:"index"`
}

func (sourceShareMigration) TableName() string {
	return "custom_db_source_shares"
}

const (
	maxVirtualIdentLength = 120
)

var (
	ErrSourceNotFound           = errors.New("data source not found")
	ErrNotSourceOwner           = errors.New("only data source owner can manage this source")
	ErrSourceShareNotFound      = errors.New("data source share not found")
	ErrSourceShareDenied        = errors.New("permission denied for this data source share operation")
	ErrSourceOrgNotFound        = errors.New("organization not found")
	ErrSourceTenantNotInOrg     = errors.New("tenant is not a member of this organization")
	ErrSourceOrgRoleCannotShare = errors.New("只有空间管理员或可编辑成员可以共享数据源到该空间")
	ErrInvalidSharePermission   = errors.New("数据源共享权限仅支持只读或可编辑")
	ErrSourceConnectionInvalid  = errors.New("database source connection invalid")
	ErrSourceReadOnlyRequired   = errors.New("database source read-only account required")
)

type sourceValidationError struct {
	kind    error
	message string
}

func (e sourceValidationError) Error() string {
	return e.message
}

func (e sourceValidationError) Unwrap() error {
	return e.kind
}

func newSourceValidationError(kind error, message string) error {
	return sourceValidationError{kind: kind, message: message}
}

func NewService(db *gorm.DB, duckdb *sql.DB) *Service {
	return &Service{db: db, duckdb: duckdb}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	db := s.db.Session(&gorm.Session{NewDB: true})
	config := *db.Config
	config.DisableForeignKeyConstraintWhenMigrating = true
	db.Config = &config
	if err := db.WithContext(ctx).AutoMigrate(
		&Source{},
		&SourceTable{},
		&SourceColumn{},
		&TableRelation{},
		&AgentBinding{},
		&sourceShareMigration{},
		&QueryAudit{},
	); err != nil {
		return err
	}
	return db.WithContext(ctx).
		Model(&sourceShareMigration{}).
		Where("permission = ?", types.OrgRoleAdmin).
		Update("permission", types.OrgRoleEditor).Error
}

func sourceApplyTenantRoleCap(p types.OrgMemberRole, callerTenantRole types.TenantRole) types.OrgMemberRole {
	if callerTenantRole == types.TenantRoleViewer && p.HasPermission(types.OrgRoleEditor) {
		return types.OrgRoleViewer
	}
	return p
}

func isValidSourceSharePermission(permission types.OrgMemberRole) bool {
	return permission == types.OrgRoleViewer || permission == types.OrgRoleEditor
}

func normalizeSourceSharePermission(permission types.OrgMemberRole) types.OrgMemberRole {
	if permission == types.OrgRoleAdmin {
		return types.OrgRoleEditor
	}
	return permission
}

func effectiveSourceSharePermission(sharePermission, memberRole types.OrgMemberRole, callerTenantRole types.TenantRole) types.OrgMemberRole {
	effective := types.MinOrgRole(normalizeSourceSharePermission(sharePermission), memberRole)
	return sourceApplyTenantRoleCap(effective, callerTenantRole)
}

func (s *Service) getTenantMember(ctx context.Context, orgID string, tenantID uint64) (*types.OrganizationTenantMember, error) {
	var member types.OrganizationTenantMember
	if err := s.db.WithContext(ctx).
		Where("organization_id = ? AND tenant_id = ?", orgID, tenantID).
		First(&member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSourceTenantNotInOrg
		}
		return nil, err
	}
	return &member, nil
}

func (s *Service) ensureOrganization(ctx context.Context, orgID string) (*types.Organization, error) {
	var org types.Organization
	if err := s.db.WithContext(ctx).Where("id = ?", orgID).First(&org).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSourceOrgNotFound
		}
		return nil, err
	}
	return &org, nil
}

func sourceResponseWithShare(src Source, share SourceShare, orgName string, permission types.OrgMemberRole, isMine bool) SourceResponse {
	resp := src.Response(false)
	resp.Shared = !isMine
	resp.ShareID = share.ID
	resp.OrganizationID = share.OrganizationID
	resp.OrgName = orgName
	resp.Permission = permission
	resp.SourceTenantID = share.SourceTenantID
	resp.IsMine = isMine
	return resp
}

func (s *Service) ListSources(ctx context.Context, tenantID uint64, callerTenantRole types.TenantRole) ([]SourceResponse, error) {
	var sources []Source
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at DESC").Find(&sources).Error; err != nil {
		return nil, err
	}
	out := make([]SourceResponse, 0, len(sources))
	for _, src := range sources {
		resp := src.Response(false)
		resp.IsMine = true
		resp.SourceTenantID = src.TenantID
		out = append(out, resp)
	}
	shared, err := s.ListSharedSources(ctx, tenantID, callerTenantRole)
	if err != nil {
		return nil, err
	}
	for _, item := range shared {
		if item == nil || item.Source == nil {
			continue
		}
		out = append(out, *item.Source)
	}
	return out, nil
}

func (s *Service) GetSource(ctx context.Context, tenantID uint64, id string) (*Source, error) {
	var src Source
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&src).Error; err != nil {
		return nil, err
	}
	return &src, nil
}

func (s *Service) GetAccessibleSource(ctx context.Context, tenantID uint64, sourceID string, callerTenantRole types.TenantRole) (*Source, *SourceShare, types.OrgMemberRole, error) {
	src, err := s.GetSource(ctx, tenantID, sourceID)
	if err == nil {
		return src, nil, types.OrgRoleAdmin, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, "", err
	}
	var members []types.OrganizationTenantMember
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Find(&members).Error; err != nil {
		return nil, nil, "", err
	}
	if len(members) == 0 {
		return nil, nil, "", gorm.ErrRecordNotFound
	}
	memberByOrg := make(map[string]types.OrganizationTenantMember, len(members))
	orgIDs := make([]string, 0, len(members))
	for _, member := range members {
		memberByOrg[member.OrganizationID] = member
		orgIDs = append(orgIDs, member.OrganizationID)
	}
	var shares []SourceShare
	if err := s.db.WithContext(ctx).
		Preload("Source").
		Preload("Organization").
		Where("source_id = ? AND organization_id IN ?", sourceID, orgIDs).
		Order("updated_at DESC").
		Find(&shares).Error; err != nil {
		return nil, nil, "", err
	}
	for _, share := range shares {
		if share.Source == nil || share.SourceTenantID == tenantID {
			continue
		}
		member := memberByOrg[share.OrganizationID]
		effective := effectiveSourceSharePermission(share.Permission, member.Role, callerTenantRole)
		if !effective.HasPermission(types.OrgRoleViewer) {
			continue
		}
		return share.Source, &share, effective, nil
	}
	var agentShares []types.AgentShare
	if err := s.db.WithContext(ctx).
		Preload("Agent").
		Preload("Organization").
		Where("organization_id IN ?", orgIDs).
		Order("updated_at DESC").
		Find(&agentShares).Error; err != nil {
		return nil, nil, "", err
	}
	for _, share := range agentShares {
		if share.Agent == nil ||
			share.Agent.Config.AgentType != types.AgentTypeDataAnalysis ||
			share.SourceTenantID == tenantID ||
			!containsString(uniqueStrings(share.Agent.Config.DBDataSources), sourceID) {
			continue
		}
		member := memberByOrg[share.OrganizationID]
		effective := types.MinOrgRole(share.Permission, member.Role)
		effective = sourceApplyTenantRoleCap(effective, callerTenantRole)
		if !effective.HasPermission(types.OrgRoleViewer) {
			continue
		}
		var src Source
		err := s.db.WithContext(ctx).
			Where("tenant_id = ? AND id = ?", share.SourceTenantID, sourceID).
			First(&src).Error
		if err == nil {
			return &src, nil, effective, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, "", err
		}
	}
	return nil, nil, "", gorm.ErrRecordNotFound
}

func (s *Service) GetSourceWithPermission(ctx context.Context, tenantID uint64, sourceID string, callerTenantRole types.TenantRole, required types.OrgMemberRole) (*Source, error) {
	src, _, permission, err := s.GetAccessibleSource(ctx, tenantID, sourceID, callerTenantRole)
	if err != nil {
		return nil, err
	}
	if !permission.HasPermission(required) {
		return nil, ErrSourceShareDenied
	}
	return src, nil
}

func (s *Service) ShareSource(ctx context.Context, sourceID, orgID, userID string, tenantID uint64, permission types.OrgMemberRole) (*SourceShare, error) {
	if _, err := s.GetSource(ctx, tenantID, sourceID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSourceNotFound
		}
		return nil, err
	}
	if _, err := s.ensureOrganization(ctx, orgID); err != nil {
		return nil, err
	}
	member, err := s.getTenantMember(ctx, orgID, tenantID)
	if err != nil {
		return nil, err
	}
	if !member.Role.HasPermission(types.OrgRoleEditor) {
		return nil, ErrSourceOrgRoleCannotShare
	}
	if !isValidSourceSharePermission(permission) {
		return nil, ErrInvalidSharePermission
	}

	var existing SourceShare
	err = s.db.WithContext(ctx).
		Where("source_id = ? AND source_tenant_id = ? AND organization_id = ?", sourceID, tenantID, orgID).
		First(&existing).Error
	if err == nil {
		existing.Permission = permission
		existing.SharedByUserID = userID
		existing.UpdatedAt = time.Now()
		if err := s.db.WithContext(ctx).Save(&existing).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	share := &SourceShare{
		SourceID:       sourceID,
		OrganizationID: orgID,
		SharedByUserID: userID,
		SourceTenantID: tenantID,
		Permission:     permission,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.db.WithContext(ctx).Create(share).Error; err != nil {
		return nil, err
	}
	return share, nil
}

func (s *Service) UpdateSharePermission(ctx context.Context, shareID string, permission types.OrgMemberRole, userID string, tenantID uint64) error {
	var share SourceShare
	if err := s.db.WithContext(ctx).Where("id = ?", shareID).First(&share).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrSourceShareNotFound
		}
		return err
	}
	if !s.callerCanManageSourceShare(ctx, share.SharedByUserID, share.SourceTenantID, share.OrganizationID, userID, tenantID) {
		return ErrSourceShareDenied
	}
	if !isValidSourceSharePermission(permission) {
		return ErrInvalidSharePermission
	}
	return s.db.WithContext(ctx).Model(&share).Updates(map[string]any{
		"permission": permission,
		"updated_at": time.Now(),
	}).Error
}

func (s *Service) RemoveShare(ctx context.Context, shareID, userID string, tenantID uint64) error {
	var share SourceShare
	if err := s.db.WithContext(ctx).Where("id = ?", shareID).First(&share).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrSourceShareNotFound
		}
		return err
	}
	if !s.callerCanManageSourceShare(ctx, share.SharedByUserID, share.SourceTenantID, share.OrganizationID, userID, tenantID) {
		return ErrSourceShareDenied
	}
	return s.db.WithContext(ctx).Delete(&share).Error
}

func (s *Service) callerCanManageSourceShare(ctx context.Context, sharedByUserID string, sourceTenantID uint64, orgID string, callerUserID string, callerTenantID uint64) bool {
	if sharedByUserID == callerUserID {
		return true
	}
	if callerTenantID != 0 && callerTenantID == sourceTenantID {
		role := types.TenantRoleFromContext(ctx)
		if role.HasPermission(types.TenantRoleAdmin) {
			return true
		}
	}
	member, err := s.getTenantMember(ctx, orgID, callerTenantID)
	return err == nil && member.Role == types.OrgRoleAdmin
}

func (s *Service) ListSharesBySource(ctx context.Context, sourceID string, tenantID uint64) ([]SourceShareResponse, error) {
	src, err := s.GetSource(ctx, tenantID, sourceID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSourceNotFound
		}
		return nil, err
	}
	var shares []SourceShare
	if err := s.db.WithContext(ctx).
		Preload("Organization").
		Preload("SharedByUser").
		Where("source_id = ? AND source_tenant_id = ?", sourceID, tenantID).
		Order("created_at DESC").
		Find(&shares).Error; err != nil {
		return nil, err
	}
	out := make([]SourceShareResponse, 0, len(shares))
	for _, share := range shares {
		memberRole := ""
		if member, err := s.getTenantMember(ctx, share.OrganizationID, tenantID); err == nil {
			memberRole = string(member.Role)
		}
		sharePermission := normalizeSourceSharePermission(share.Permission)
		resp := SourceShareResponse{
			ID:             share.ID,
			SourceID:       src.ID,
			SourceName:     src.Name,
			SourceType:     src.Type,
			OrganizationID: share.OrganizationID,
			SharedByUserID: share.SharedByUserID,
			SourceTenantID: share.SourceTenantID,
			Permission:     string(sharePermission),
			MyRoleInOrg:    memberRole,
			MyPermission:   string(types.MinOrgRole(sharePermission, types.OrgMemberRole(memberRole))),
			CreatedAt:      share.CreatedAt,
		}
		if share.Organization != nil {
			resp.OrganizationName = share.Organization.Name
			resp.RequireApproval = share.Organization.RequireApproval
		}
		if share.SharedByUser != nil {
			resp.SharedByUsername = share.SharedByUser.DisplayNameOrUsername()
		}
		out = append(out, resp)
	}
	return out, nil
}

func (s *Service) ListSharedSources(ctx context.Context, tenantID uint64, callerTenantRole types.TenantRole) ([]*SharedSourceInfo, error) {
	var members []types.OrganizationTenantMember
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Find(&members).Error; err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return []*SharedSourceInfo{}, nil
	}
	memberByOrg := make(map[string]types.OrganizationTenantMember, len(members))
	orgIDs := make([]string, 0, len(members))
	for _, member := range members {
		memberByOrg[member.OrganizationID] = member
		orgIDs = append(orgIDs, member.OrganizationID)
	}
	var shares []SourceShare
	if err := s.db.WithContext(ctx).
		Preload("Source").
		Preload("Organization").
		Where("organization_id IN ?", orgIDs).
		Order("updated_at DESC").
		Find(&shares).Error; err != nil {
		return nil, err
	}

	bySource := map[string]*SharedSourceInfo{}
	for _, share := range shares {
		if share.Source == nil || share.SourceTenantID == tenantID {
			continue
		}
		member := memberByOrg[share.OrganizationID]
		effective := effectiveSourceSharePermission(share.Permission, member.Role, callerTenantRole)
		orgName := ""
		if share.Organization != nil {
			orgName = share.Organization.Name
		}
		resp := sourceResponseWithShare(*share.Source, share, orgName, effective, false)
		info := &SharedSourceInfo{
			Source:         &resp,
			ShareID:        share.ID,
			OrganizationID: share.OrganizationID,
			OrgName:        orgName,
			Permission:     effective,
			SourceTenantID: share.SourceTenantID,
			SharedAt:       share.CreatedAt,
		}
		existing, ok := bySource[share.SourceID]
		if !ok || effective.HasPermission(existing.Permission) && effective != existing.Permission {
			bySource[share.SourceID] = info
		}
	}
	out := make([]*SharedSourceInfo, 0, len(bySource))
	for _, item := range bySource {
		out = append(out, item)
	}
	return out, nil
}

func (s *Service) ListSharedSourcesInOrganization(ctx context.Context, orgID string, tenantID uint64, callerTenantRole types.TenantRole) ([]*OrganizationSharedSourceItem, error) {
	member, err := s.getTenantMember(ctx, orgID, tenantID)
	if err != nil {
		return nil, err
	}
	var shares []SourceShare
	if err := s.db.WithContext(ctx).
		Preload("Source").
		Preload("Organization").
		Where("organization_id = ?", orgID).
		Order("created_at DESC").
		Find(&shares).Error; err != nil {
		return nil, err
	}
	out := make([]*OrganizationSharedSourceItem, 0, len(shares))
	for _, share := range shares {
		if share.Source == nil {
			continue
		}
		effective := effectiveSourceSharePermission(share.Permission, member.Role, callerTenantRole)
		orgName := ""
		if share.Organization != nil {
			orgName = share.Organization.Name
		}
		isMine := share.SourceTenantID == tenantID
		resp := sourceResponseWithShare(*share.Source, share, orgName, effective, isMine)
		out = append(out, &OrganizationSharedSourceItem{
			SharedSourceInfo: SharedSourceInfo{
				Source:         &resp,
				ShareID:        share.ID,
				OrganizationID: share.OrganizationID,
				OrgName:        orgName,
				Permission:     effective,
				SourceTenantID: share.SourceTenantID,
				SharedAt:       share.CreatedAt,
			},
			IsMine: isMine,
		})
	}
	return out, nil
}

func (s *Service) ListOrganizationSourcesIncludingAgent(ctx context.Context, orgID string, tenantID uint64, callerTenantRole types.TenantRole) ([]*OrganizationSharedSourceItem, error) {
	direct, err := s.ListSharedSourcesInOrganization(ctx, orgID, tenantID, callerTenantRole)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(direct))
	for _, item := range direct {
		if item != nil && item.Source != nil {
			seen[item.Source.ID] = true
		}
	}
	orgName := ""
	if org, err := s.ensureOrganization(ctx, orgID); err == nil && org != nil {
		orgName = org.Name
	}
	var shares []types.AgentShare
	if err := s.db.WithContext(ctx).
		Preload("Agent").
		Preload("Organization").
		Where("organization_id = ?", orgID).
		Order("created_at DESC").
		Find(&shares).Error; err != nil {
		return direct, nil
	}
	merged := make([]*OrganizationSharedSourceItem, 0, len(direct)+len(shares))
	merged = append(merged, direct...)
	for _, share := range shares {
		if share.Agent == nil || share.Agent.Config.AgentType != types.AgentTypeDataAnalysis {
			continue
		}
		agentName := share.Agent.Name
		if agentName == "" {
			agentName = share.Agent.ID
		}
		sourceTenantID := share.Agent.TenantID
		for _, sourceID := range uniqueStrings(share.Agent.Config.DBDataSources) {
			if sourceID == "" || seen[sourceID] {
				continue
			}
			var src Source
			err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", sourceTenantID, sourceID).First(&src).Error
			if err != nil {
				continue
			}
			seen[sourceID] = true
			resp := src.Response(false)
			resp.Shared = sourceTenantID != tenantID
			resp.OrganizationID = orgID
			resp.OrgName = orgName
			resp.Permission = types.OrgRoleViewer
			resp.SourceTenantID = sourceTenantID
			resp.IsMine = sourceTenantID == tenantID
			resp.SourceFromAgent = &SourceFromAgentInfo{AgentID: share.Agent.ID, AgentName: agentName}
			merged = append(merged, &OrganizationSharedSourceItem{
				SharedSourceInfo: SharedSourceInfo{
					Source:         &resp,
					OrganizationID: orgID,
					OrgName:        orgName,
					Permission:     types.OrgRoleViewer,
					SourceTenantID: sourceTenantID,
					SharedAt:       share.CreatedAt,
				},
				IsMine:          sourceTenantID == tenantID,
				SourceFromAgent: resp.SourceFromAgent,
			})
		}
	}
	return merged, nil
}

func (s *Service) CreateSource(ctx context.Context, tenantID uint64, userID string, req CreateSourceRequest) (*Source, error) {
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	if req.Type != SourceTypeMySQL && req.Type != SourceTypePostgres {
		return nil, newSourceValidationError(ErrSourceConnectionInvalid, "仅支持 MySQL 和 PostgreSQL 数据源")
	}
	normalizeSourceRequest(&req)
	conn, err := connectorFor(req.Type)
	if err != nil {
		return nil, err
	}
	if err := s.validateSourceAccess(ctx, conn, req.Config, sourceTimeout(req.TimeoutSeconds)); err != nil {
		return nil, err
	}
	src := &Source{
		TenantID: tenantID, Name: strings.TrimSpace(req.Name), Description: strings.TrimSpace(req.Description),
		Type: req.Type, Status: SourceStatusActive, QueryMode: req.QueryMode, MaxRows: req.MaxRows,
		MaxScanRows: req.MaxScanRows, TimeoutSeconds: req.TimeoutSeconds, CreatedBy: userID,
	}
	if err := src.SetConfig(req.Config); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Create(src).Error; err != nil {
		return nil, err
	}
	if err := s.RefreshMetadata(ctx, tenantID, src.ID, types.TenantRoleOwner); err != nil {
		logger.Warnf(ctx, "[dbanalytics] metadata refresh after create failed: %v", err)
		_ = s.db.WithContext(ctx).Model(src).Updates(map[string]any{
			"status": SourceStatusError, "error_message": err.Error(),
		}).Error
		src.Status = SourceStatusError
		src.ErrorMessage = err.Error()
	}
	return src, nil
}

func (s *Service) TestSourceConfig(ctx context.Context, req TestSourceRequest) error {
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	if req.Type != SourceTypeMySQL && req.Type != SourceTypePostgres {
		return newSourceValidationError(ErrSourceConnectionInvalid, "仅支持 MySQL 和 PostgreSQL 数据源")
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 30
	}
	normalizeSourceConfig(req.Type, &req.Config)
	conn, err := connectorFor(req.Type)
	if err != nil {
		return err
	}
	return s.validateSourceAccess(ctx, conn, req.Config, sourceTimeout(req.TimeoutSeconds))
}

func normalizeSourceRequest(req *CreateSourceRequest) {
	if req.QueryMode == "" {
		req.QueryMode = QueryModeLive
	}
	if req.MaxRows <= 0 {
		req.MaxRows = 1000
	}
	if req.MaxScanRows <= 0 {
		req.MaxScanRows = 50000
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 30
	}
	normalizeSourceConfig(req.Type, &req.Config)
}

func normalizeSourceConfig(sourceType string, cfg *SourceConfig) {
	if cfg == nil {
		return
	}
	if cfg.Port == 0 {
		if sourceType == SourceTypeMySQL {
			cfg.Port = 3306
		} else {
			cfg.Port = 5432
		}
	}
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Database = strings.TrimSpace(cfg.Database)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.SSLMode = strings.TrimSpace(cfg.SSLMode)
}

func (s *Service) UpdateSource(ctx context.Context, tenantID uint64, id string, callerTenantRole types.TenantRole, req UpdateSourceRequest) (*Source, error) {
	src, err := s.GetSourceWithPermission(ctx, tenantID, id, callerTenantRole, types.OrgRoleEditor)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{}
	if strings.TrimSpace(req.Name) != "" {
		updates["name"] = strings.TrimSpace(req.Name)
	}
	updates["description"] = strings.TrimSpace(req.Description)
	if req.QueryMode != "" {
		updates["query_mode"] = req.QueryMode
	}
	if req.MaxRows > 0 {
		updates["max_rows"] = req.MaxRows
	}
	if req.MaxScanRows > 0 {
		updates["max_scan_rows"] = req.MaxScanRows
	}
	if req.TimeoutSeconds > 0 {
		updates["timeout_seconds"] = req.TimeoutSeconds
	}
	if req.Config != nil {
		cfg, err := src.ParseConfig()
		if err != nil {
			return nil, err
		}
		next := *req.Config
		if strings.TrimSpace(next.Host) != "" {
			cfg.Host = strings.TrimSpace(next.Host)
		}
		if next.Port > 0 {
			cfg.Port = next.Port
		}
		if strings.TrimSpace(next.Database) != "" {
			cfg.Database = strings.TrimSpace(next.Database)
		}
		if strings.TrimSpace(next.Username) != "" {
			cfg.Username = strings.TrimSpace(next.Username)
		}
		if next.Password != "" {
			cfg.Password = next.Password
		}
		cfg.SSLMode = strings.TrimSpace(next.SSLMode)
		if next.Params != nil {
			cfg.Params = next.Params
		}
		if cfg.Port == 0 {
			if src.Type == SourceTypeMySQL {
				cfg.Port = 3306
			} else {
				cfg.Port = 5432
			}
		}
		normalizeSourceConfig(src.Type, &cfg)
		conn, err := connectorFor(src.Type)
		if err != nil {
			return nil, err
		}
		timeout := sourceTimeout(src.TimeoutSeconds)
		if req.TimeoutSeconds > 0 {
			timeout = sourceTimeout(req.TimeoutSeconds)
		}
		if err := s.validateSourceAccess(ctx, conn, cfg, timeout); err != nil {
			return nil, err
		}
		tmp := *src
		if err := tmp.SetConfig(cfg); err != nil {
			return nil, err
		}
		updates["config"] = tmp.Config
	}
	updates["status"] = SourceStatusActive
	updates["error_message"] = ""
	if err := s.db.WithContext(ctx).Model(src).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetSource(ctx, src.TenantID, id)
}

func (s *Service) DeleteSource(ctx context.Context, tenantID uint64, id string) error {
	src, err := s.GetSource(ctx, tenantID, id)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ? AND source_id = ?", tenantID, id).Delete(&SourceColumn{}).Error; err != nil {
			return err
		}
		if err := tx.Where("tenant_id = ? AND source_id = ?", tenantID, id).Delete(&SourceTable{}).Error; err != nil {
			return err
		}
		if err := tx.Where("tenant_id = ? AND source_id = ?", tenantID, id).Delete(&AgentBinding{}).Error; err != nil {
			return err
		}
		if err := tx.Where("source_tenant_id = ? AND source_id = ?", tenantID, id).Delete(&SourceShare{}).Error; err != nil {
			return err
		}
		return tx.Delete(src).Error
	})
}

func (s *Service) TestConnection(ctx context.Context, tenantID uint64, id string, callerTenantRole types.TenantRole) error {
	src, err := s.GetSourceWithPermission(ctx, tenantID, id, callerTenantRole, types.OrgRoleEditor)
	if err != nil {
		return err
	}
	cfg, err := src.ParseConfig()
	if err != nil {
		return err
	}
	conn, err := connectorFor(src.Type)
	if err != nil {
		return err
	}
	err = s.validateSourceAccess(ctx, conn, cfg, sourceTimeout(src.TimeoutSeconds))
	updates := map[string]any{"status": SourceStatusActive, "error_message": ""}
	if err != nil {
		updates["status"] = SourceStatusError
		updates["error_message"] = err.Error()
	}
	_ = s.db.WithContext(ctx).Model(src).Updates(updates).Error
	return err
}

func (s *Service) validateSourceAccess(ctx context.Context, conn Connector, cfg SourceConfig, timeout time.Duration) error {
	if err := conn.Validate(ctx, cfg, timeout); err != nil {
		return newSourceValidationError(
			ErrSourceConnectionInvalid,
			"连接测试失败："+sanitizeError(err),
		)
	}
	if err := conn.ValidateReadOnly(ctx, cfg, timeout); err != nil {
		return newSourceValidationError(
			ErrSourceReadOnlyRequired,
			"账号权限校验失败："+sanitizeError(err)+"；请使用只有读取权限的数据库账号",
		)
	}
	return nil
}

func (s *Service) ListSchemas(ctx context.Context, tenantID uint64, sourceID string, callerTenantRole types.TenantRole) ([]string, error) {
	src, err := s.GetSourceWithPermission(ctx, tenantID, sourceID, callerTenantRole, types.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	cfg, err := src.ParseConfig()
	if err != nil {
		return nil, err
	}
	conn, err := connectorFor(src.Type)
	if err != nil {
		return nil, err
	}
	return conn.ListSchemas(ctx, cfg, sourceTimeout(src.TimeoutSeconds))
}

func (s *Service) ListTables(ctx context.Context, tenantID uint64, sourceID, schema string, callerTenantRole types.TenantRole) ([]SourceTable, error) {
	src, err := s.GetSourceWithPermission(ctx, tenantID, sourceID, callerTenantRole, types.OrgRoleViewer)
	if err != nil {
		return nil, err
	}
	if schema == "" {
		schema = defaultSchema(src)
	}
	var tables []SourceTable
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ? AND schema_name = ?", src.TenantID, sourceID, schema).
		Order("table_name ASC").Find(&tables).Error; err != nil {
		return nil, err
	}
	return tables, nil
}

func (s *Service) RefreshMetadata(ctx context.Context, tenantID uint64, sourceID string, callerTenantRole types.TenantRole) error {
	src, err := s.GetSourceWithPermission(ctx, tenantID, sourceID, callerTenantRole, types.OrgRoleEditor)
	if err != nil {
		return err
	}
	cfg, err := src.ParseConfig()
	if err != nil {
		return err
	}
	conn, err := connectorFor(src.Type)
	if err != nil {
		return err
	}
	schemas, err := conn.ListSchemas(ctx, cfg, sourceTimeout(src.TimeoutSeconds))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, schema := range schemas {
		profiles, err := conn.ListTables(ctx, cfg, schema, sourceTimeout(src.TimeoutSeconds))
		if err != nil {
			logger.Warnf(ctx, "[dbanalytics] list tables failed source=%s schema=%s: %v", sourceID, schema, err)
			continue
		}
		for _, profile := range profiles {
			virtual := virtualTableName(src, profile.SchemaName, profile.TableName)
			table := SourceTable{
				SourceID: src.ID, TenantID: src.TenantID, SchemaName: profile.SchemaName, PhysicalName: profile.TableName,
				ObjectType: profile.ObjectType, VirtualName: virtual, RowEstimate: profile.RowEstimate,
				Description: profile.Description, LastProfiledAt: &now,
			}
			var existing SourceTable
			err := s.db.WithContext(ctx).Where(
				"tenant_id = ? AND source_id = ? AND schema_name = ? AND table_name = ?",
				src.TenantID, src.ID, profile.SchemaName, profile.TableName,
			).First(&existing).Error
			if err == nil {
				table.ID = existing.ID
				table.Enabled = existing.Enabled
				if strings.TrimSpace(existing.Description) != "" {
					table.Description = existing.Description
				}
			}
			if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "source_id"}, {Name: "schema_name"}, {Name: "table_name"}},
				DoUpdates: clause.AssignmentColumns([]string{"object_type", "virtual_name", "row_estimate", "description", "last_profiled_at", "updated_at"}),
			}).Create(&table).Error; err != nil {
				return err
			}
			if table.ID == "" {
				_ = s.db.WithContext(ctx).Where(
					"tenant_id = ? AND source_id = ? AND schema_name = ? AND table_name = ?",
					src.TenantID, src.ID, profile.SchemaName, profile.TableName,
				).First(&table).Error
			}
			if err := s.refreshColumns(ctx, src, cfg, conn, &table); err != nil {
				logger.Warnf(ctx, "[dbanalytics] describe table failed source=%s table=%s: %v", sourceID, table.VirtualName, err)
			}
		}
	}
	return s.db.WithContext(ctx).Model(src).Updates(map[string]any{"status": SourceStatusActive, "error_message": ""}).Error
}

func (s *Service) refreshColumns(ctx context.Context, src *Source, cfg SourceConfig, conn Connector, table *SourceTable) error {
	ref := TableRef{SchemaName: table.SchemaName, TableName: table.PhysicalName, VirtualName: table.VirtualName}
	profile, err := conn.DescribeTable(ctx, cfg, ref, sourceTimeout(src.TimeoutSeconds))
	if err != nil {
		return err
	}
	samples, _ := conn.SampleRows(ctx, cfg, ref, 5, sourceTimeout(src.TimeoutSeconds))
	existing := map[string]SourceColumn{}
	var existingCols []SourceColumn
	_ = s.db.WithContext(ctx).Where("tenant_id = ? AND table_id = ?", table.TenantID, table.ID).Find(&existingCols).Error
	for _, col := range existingCols {
		existing[strings.ToLower(col.ColumnName)] = col
	}
	for _, col := range profile.Columns {
		sampleValues := sampleValuesForColumn(samples, col.Name)
		row := SourceColumn{
			TableID: table.ID, SourceID: table.SourceID, TenantID: table.TenantID, ColumnName: col.Name,
			DataType: col.DataType, Nullable: col.Nullable, Ordinal: col.Ordinal,
			Description:  col.Description,
			SemanticType: col.SemanticType, SensitiveLevel: "none",
		}
		if old, ok := existing[strings.ToLower(col.Name)]; ok {
			row.ID = old.ID
			if strings.TrimSpace(old.Description) != "" {
				row.Description = old.Description
			}
			if strings.TrimSpace(old.SemanticType) != "" {
				row.SemanticType = old.SemanticType
			}
			if strings.TrimSpace(old.SensitiveLevel) != "" {
				row.SensitiveLevel = old.SensitiveLevel
			}
		}
		if isMaskedColumn(row) {
			sampleValues = maskStringSamples(sampleValues)
		}
		sampleJSON, _ := json.Marshal(sampleValues)
		row.SampleValues = types.JSON(sampleJSON)
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "table_id"}, {Name: "column_name"}},
			DoUpdates: clause.AssignmentColumns([]string{"data_type", "nullable", "ordinal", "description", "sample_values", "semantic_type", "sensitive_level", "updated_at"}),
		}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) SetTableScope(ctx context.Context, tenantID uint64, sourceID string, callerTenantRole types.TenantRole, tableIDs []string) error {
	src, err := s.GetSourceWithPermission(ctx, tenantID, sourceID, callerTenantRole, types.OrgRoleEditor)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SourceTable{}).Where("tenant_id = ? AND source_id = ?", src.TenantID, sourceID).Update("enabled", false).Error; err != nil {
			return err
		}
		if len(tableIDs) == 0 {
			return nil
		}
		return tx.Model(&SourceTable{}).Where("tenant_id = ? AND source_id = ? AND id IN ?", src.TenantID, sourceID, tableIDs).Update("enabled", true).Error
	})
}

func (s *Service) UpdateColumn(ctx context.Context, tenantID uint64, columnID string, callerTenantRole types.TenantRole, req UpdateColumnRequest) (*SourceColumn, error) {
	var col SourceColumn
	if err := s.db.WithContext(ctx).Where("id = ?", columnID).First(&col).Error; err != nil {
		return nil, err
	}
	if _, err := s.GetSourceWithPermission(ctx, tenantID, col.SourceID, callerTenantRole, types.OrgRoleEditor); err != nil {
		return nil, err
	}
	updates := map[string]any{
		"description": strings.TrimSpace(req.Description),
	}
	if req.SemanticType != "" {
		updates["semantic_type"] = strings.TrimSpace(req.SemanticType)
	}
	if req.SensitiveLevel != "" {
		sensitiveLevel := strings.TrimSpace(req.SensitiveLevel)
		updates["sensitive_level"] = sensitiveLevel
		if isMaskedSensitiveLevel(sensitiveLevel) {
			sampleJSON, _ := json.Marshal(maskStringSamples(parseStringArray(col.SampleValues)))
			updates["sample_values"] = types.JSON(sampleJSON)
		}
	}
	if err := s.db.WithContext(ctx).Model(&col).Updates(updates).Error; err != nil {
		return nil, err
	}
	_ = s.db.WithContext(ctx).First(&col, "id = ?", columnID).Error
	maskColumnForResponse(&col)
	return &col, nil
}

func (s *Service) GetSourceWithTables(ctx context.Context, tenantID uint64, sourceID string) (*Source, error) {
	return s.GetAccessibleSourceWithTables(ctx, tenantID, sourceID, types.TenantRoleOwner)
}

func (s *Service) GetAccessibleSourceWithTables(ctx context.Context, tenantID uint64, sourceID string, callerTenantRole types.TenantRole) (*Source, error) {
	src, _, permission, err := s.GetAccessibleSource(ctx, tenantID, sourceID, callerTenantRole)
	if err != nil {
		return nil, err
	}
	if !permission.HasPermission(types.OrgRoleViewer) {
		return nil, ErrSourceShareDenied
	}
	var full Source
	if err := s.db.WithContext(ctx).Preload("Tables", func(db *gorm.DB) *gorm.DB {
		return db.Order("schema_name ASC, table_name ASC")
	}).Where("tenant_id = ? AND id = ?", src.TenantID, sourceID).First(&full).Error; err != nil {
		return nil, err
	}
	for i := range full.Tables {
		_ = s.db.WithContext(ctx).Where("tenant_id = ? AND table_id = ?", full.TenantID, full.Tables[i].ID).Order("ordinal ASC").Find(&full.Tables[i].Columns).Error
	}
	maskSourceColumnsForResponse(&full)
	return &full, nil
}

func (s *Service) SetAgentBindings(ctx context.Context, tenantID uint64, agentID string, sourceIDs []string) error {
	sourceIDs = uniqueStrings(sourceIDs)
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ? AND agent_id = ?", tenantID, agentID).Delete(&AgentBinding{}).Error; err != nil {
			return err
		}
		for _, sourceID := range sourceIDs {
			if strings.TrimSpace(sourceID) == "" {
				continue
			}
			var count int64
			if err := tx.Model(&Source{}).Where("tenant_id = ? AND id = ?", tenantID, sourceID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("source not found: %s", sourceID)
			}
			if err := tx.Create(&AgentBinding{TenantID: tenantID, AgentID: agentID, SourceID: sourceID, Enabled: true}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) GetAgentBindings(ctx context.Context, tenantID uint64, agentID string) ([]string, error) {
	var rows []AgentBinding
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND agent_id = ? AND enabled = ?", tenantID, agentID, true).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.SourceID)
	}
	return out, nil
}

type ToolScope struct {
	TenantID       uint64
	SourceTenantID uint64
	UserID         string
	SessionID      string
	TenantRole     types.TenantRole
	AgentID        string
	AgentType      string
	SourceIDs      []string
}

func (s *Service) Catalog(ctx context.Context, scope ToolScope, input CatalogInput) (map[string]any, error) {
	sources, tables, err := s.loadEnabledTables(ctx, scope, input.SourceID)
	if err != nil {
		return nil, err
	}
	queryTerms := tokenize(input.Query)
	type scored struct {
		Score  int
		Source Source
		Table  SourceTable
	}
	var ranked []scored
	for _, table := range tables {
		score := scoreTable(table, queryTerms)
		if score > 0 || len(queryTerms) == 0 {
			ranked = append(ranked, scored{Score: score, Source: sources[table.SourceID], Table: table})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			return ranked[i].Table.VirtualName < ranked[j].Table.VirtualName
		}
		return ranked[i].Score > ranked[j].Score
	})
	if len(ranked) > 30 {
		ranked = ranked[:30]
	}
	items := make([]map[string]any, 0, len(ranked))
	for _, item := range ranked {
		cols := make([]map[string]any, 0, len(item.Table.Columns))
		for _, col := range item.Table.Columns {
			if isHiddenColumn(col) {
				continue
			}
			cols = append(cols, map[string]any{
				"name": col.ColumnName, "type": col.DataType, "description": col.Description,
				"semantic_type": col.SemanticType, "sample_values": maskedColumnSampleValues(col),
			})
		}
		items = append(items, map[string]any{
			"source_id": item.Source.ID, "source_name": item.Source.Name, "source_type": item.Source.Type,
			"schema_name": item.Table.SchemaName, "table_name": item.Table.PhysicalName,
			"sql_table_name": item.Table.VirtualName, "object_type": item.Table.ObjectType,
			"description": item.Table.Description, "row_estimate": item.Table.RowEstimate,
			"columns": cols,
		})
	}
	return map[string]any{
		"display_type": "db_catalog", "query": input.Query, "sources": sourceSummaries(sources), "tables": items, "count": len(items),
	}, nil
}

func (s *Service) Schema(ctx context.Context, scope ToolScope, input SchemaInput) (map[string]any, error) {
	sources, tables, err := s.loadEnabledTables(ctx, scope, input.SourceID)
	if err != nil {
		return nil, err
	}
	filter := map[string]bool{}
	for _, name := range input.TableNames {
		filter[strings.ToLower(strings.TrimSpace(name))] = true
	}
	outTables := make([]map[string]any, 0, len(tables))
	selectedTables := make([]SourceTable, 0, len(tables))
	for _, table := range tables {
		if len(filter) > 0 && !matchesTableFilter(table, filter) {
			continue
		}
		selectedTables = append(selectedTables, table)
		cols := make([]map[string]any, 0, len(table.Columns))
		for _, col := range table.Columns {
			if isHiddenColumn(col) {
				continue
			}
			cols = append(cols, map[string]any{
				"name": col.ColumnName, "type": col.DataType, "nullable": col.Nullable,
				"description": col.Description, "semantic_type": col.SemanticType,
				"sensitive_level": col.SensitiveLevel, "sample_values": maskedColumnSampleValues(col),
			})
		}
		src := sources[table.SourceID]
		outTables = append(outTables, map[string]any{
			"source_id": src.ID, "source_name": src.Name, "source_type": src.Type,
			"schema_name": table.SchemaName, "table_name": table.PhysicalName, "sql_table_name": table.VirtualName,
			"description": table.Description, "row_estimate": table.RowEstimate, "columns": cols,
		})
	}
	s.markSchemaReasoning(scope, selectedTables)
	return map[string]any{
		"display_type":     "db_schema",
		"tables":           outTables,
		"semantic_context": inferBusinessSemantics(selectedTables),
		"count":            len(outTables),
	}, nil
}

func matchesTableFilter(table SourceTable, filter map[string]bool) bool {
	if len(filter) == 0 {
		return true
	}
	candidates := []string{
		table.VirtualName,
		table.PhysicalName,
		table.SchemaName + "." + table.PhysicalName,
	}
	for _, item := range candidates {
		if filter[strings.ToLower(strings.TrimSpace(item))] {
			return true
		}
	}
	return false
}

func (s *Service) ExecuteQuery(ctx context.Context, scope ToolScope, input QueryInput, allowChart bool) (map[string]any, error) {
	start := time.Now()
	audit := QueryAudit{
		TenantID: scope.TenantID, UserID: scope.UserID, AgentID: scope.AgentID, SourceID: input.SourceID,
		OriginalSQL: input.SQL, QueryMode: QueryModeLive, ChartRequested: input.ChartRequested && allowChart,
	}
	result, err := s.executeQuery(ctx, scope, input, allowChart)
	audit.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		audit.Success = false
		audit.ErrorMessage = sanitizeError(err)
		_ = s.db.WithContext(ctx).Create(&audit).Error
		return nil, err
	}
	audit.Success = true
	audit.ExecutedSQL = fmt.Sprint(result["query"])
	audit.RowCount = intFromAny(result["row_count"])
	_ = s.db.WithContext(ctx).Create(&audit).Error
	return result, nil
}

func (s *Service) executeQuery(ctx context.Context, scope ToolScope, input QueryInput, allowChart bool) (map[string]any, error) {
	if strings.TrimSpace(input.SQL) == "" {
		return nil, fmt.Errorf("sql is required")
	}
	if err := s.requireSchemaReasoning(scope); err != nil {
		return nil, err
	}
	sources, tables, err := s.loadEnabledTables(ctx, scope, input.SourceID)
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("no enabled database tables are bound to this agent")
	}
	allowedNames := make([]string, 0, len(tables))
	for _, table := range tables {
		allowedNames = append(allowedNames, table.VirtualName)
	}
	normalizedSQL := normalizeAnalysisSQL(input.SQL)
	parseResult, validation := utils.ValidateSQL(
		normalizedSQL,
		utils.WithInputValidation(6, 12000),
		utils.WithSelectOnly(),
		utils.WithSingleStatement(),
		utils.WithAllowedTables(allowedNames...),
		utils.WithNoDangerousFunctions(),
		utils.WithInjectionRiskCheck(),
	)
	if !validation.Valid {
		return nil, withDuckDBSQLHint(fmt.Errorf("SQL validation failed: %v", validation.Errors))
	}
	var referencedNames []string
	if parseResult != nil {
		referencedNames = parseResult.TableNames
	}
	queryTables := tablesReferencedBySQL(tables, referencedNames, normalizedSQL)
	if err := s.requireSchemaReasoningForTables(scope, queryTables); err != nil {
		return nil, err
	}

	conn, err := s.duckdb.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open duckdb connection: %w", err)
	}
	defer conn.Close()

	limitTables := queryTables
	if len(limitTables) == 0 {
		limitTables = tables
	}
	maxRows := effectiveMaxRows(sources, limitTables)
	maxScanRows := effectiveMaxScanRows(sources, limitTables)
	for _, table := range queryTables {
		src := sources[table.SourceID]
		cfg, err := src.ParseConfig()
		if err != nil {
			return nil, err
		}
		sourceConn, err := connectorFor(src.Type)
		if err != nil {
			return nil, err
		}
		if err := s.materializeTable(ctx, conn, sourceConn, src, cfg, table, maxScanRows); err != nil {
			return nil, err
		}
	}
	querySQL := limitOuterQuery(normalizedSQL, maxRows)
	rows, err := conn.QueryContext(ctx, querySQL)
	if err != nil {
		return nil, withDuckDBSQLHint(fmt.Errorf("query execution failed: %w", err))
	}
	defer rows.Close()
	cols, resultRows, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	chartRequested := input.ChartRequested && allowChart
	chart := inferChartSpec(cols, resultRows, input.PreferredChart, chartRequested, chartHintsFromInput(input))
	tableRequested := input.TableRequested
	displayMode := "text_only"
	if eligible, _ := chart["eligible"].(bool); eligible {
		displayMode = "chart_only"
		if tableRequested {
			displayMode = "chart_and_table"
		}
	} else if tableRequested {
		displayMode = "table"
	}
	return map[string]any{
		"display_type":    DisplayTypeStructuredAnalysis,
		"display_mode":    displayMode,
		"analysis_type":   "database",
		"source":          map[string]any{"type": "database", "source_count": len(sources)},
		"query":           querySQL,
		"columns":         cols,
		"rows":            resultRows,
		"row_count":       len(resultRows),
		"chart_requested": chartRequested,
		"chart":           chart,
		"table_requested": tableRequested,
		"table_visible":   tableRequested,
		"limits":          map[string]any{"max_rows": maxRows, "max_scan_rows": maxScanRows, "truncated": len(resultRows) >= maxRows},
	}, nil
}

func (s *Service) materializeTable(ctx context.Context, conn *sql.Conn, sourceConn Connector, src Source, cfg SourceConfig, table SourceTable, limit int) error {
	cols := table.Columns
	if len(cols) == 0 {
		if err := s.db.WithContext(ctx).Where("tenant_id = ? AND table_id = ?", table.TenantID, table.ID).Order("ordinal ASC").Find(&cols).Error; err != nil {
			return err
		}
	}
	ref := TableRef{SchemaName: table.SchemaName, TableName: table.PhysicalName, VirtualName: table.VirtualName}
	_, rows, err := sourceConn.QueryRows(ctx, cfg, ref, limit, sourceTimeout(src.TimeoutSeconds))
	if err != nil {
		return fmt.Errorf("load source table %s: %w", table.VirtualName, err)
	}
	columnDefs := make([]string, 0, len(cols))
	for _, col := range cols {
		if isHiddenColumn(col) {
			continue
		}
		columnDefs = append(columnDefs, fmt.Sprintf("%s %s", quoteDuckIdent(col.ColumnName), duckTypeForMaterializedColumn(col)))
	}
	if len(columnDefs) == 0 {
		return fmt.Errorf("table %s has no queryable columns", table.VirtualName)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", quoteDuckIdent(table.VirtualName))); err != nil {
		return fmt.Errorf("reset analysis table %s: %w", table.VirtualName, err)
	}
	createSQL := fmt.Sprintf("CREATE TEMP TABLE %s (%s)", quoteDuckIdent(table.VirtualName), strings.Join(columnDefs, ", "))
	if _, err := conn.ExecContext(ctx, createSQL); err != nil {
		return err
	}
	visibleCols := make([]SourceColumn, 0, len(cols))
	for _, col := range cols {
		if !isHiddenColumn(col) {
			visibleCols = append(visibleCols, col)
		}
	}
	placeholders := make([]string, len(visibleCols))
	colNames := make([]string, len(visibleCols))
	for i, col := range visibleCols {
		placeholders[i] = "?"
		colNames[i] = quoteDuckIdent(col.ColumnName)
	}
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quoteDuckIdent(table.VirtualName), strings.Join(colNames, ", "), strings.Join(placeholders, ", "))
	for _, row := range rows {
		values := make([]any, len(visibleCols))
		for i, col := range visibleCols {
			values[i] = materializedColumnValue(col, row[col.ColumnName])
		}
		if _, err := conn.ExecContext(ctx, insertSQL, values...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) schemaReasoningKey(scope ToolScope) string {
	sourceTenantID := scope.SourceTenantID
	if sourceTenantID == 0 {
		sourceTenantID = scope.TenantID
	}
	sourceIDs := uniqueStrings(scope.SourceIDs)
	sort.Strings(sourceIDs)
	return fmt.Sprintf("%s:%d:%s", scope.SessionID, sourceTenantID, strings.Join(sourceIDs, ","))
}

func (s *Service) schemaReasoningTableKey(scope ToolScope, tableName string) string {
	return s.schemaReasoningKey(scope) + ":table:" + strings.ToLower(strings.TrimSpace(tableName))
}

func (s *Service) markSchemaReasoning(scope ToolScope, tables []SourceTable) {
	if scope.SessionID == "" || len(tables) == 0 {
		return
	}
	s.schemaReasoning.Store(s.schemaReasoningKey(scope), true)
	for _, table := range tables {
		if strings.TrimSpace(table.VirtualName) == "" {
			continue
		}
		s.schemaReasoning.Store(s.schemaReasoningTableKey(scope, table.VirtualName), true)
	}
}

func (s *Service) requireSchemaReasoning(scope ToolScope) error {
	if scope.SessionID == "" {
		return nil
	}
	if _, ok := s.schemaReasoning.Load(s.schemaReasoningKey(scope)); ok {
		return nil
	}
	return fmt.Errorf("call db_schema first to infer table and field business meaning before db_query")
}

func (s *Service) requireSchemaReasoningForTables(scope ToolScope, tables []SourceTable) error {
	if err := s.requireSchemaReasoning(scope); err != nil {
		return err
	}
	if scope.SessionID == "" || len(tables) == 0 {
		return nil
	}
	missing := make([]string, 0)
	for _, table := range tables {
		if strings.TrimSpace(table.VirtualName) == "" {
			continue
		}
		if _, ok := s.schemaReasoning.Load(s.schemaReasoningTableKey(scope, table.VirtualName)); !ok {
			missing = append(missing, table.VirtualName)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("call db_schema for table(s) %s before db_query to infer table and field business meaning", strings.Join(missing, ", "))
	}
	return nil
}

func (s *Service) loadEnabledTables(ctx context.Context, scope ToolScope, requestedSourceID string) (map[string]Source, []SourceTable, error) {
	sourceIDs := uniqueStrings(scope.SourceIDs)
	if requestedSourceID != "" {
		if len(sourceIDs) > 0 && !containsString(sourceIDs, requestedSourceID) {
			return nil, nil, fmt.Errorf("source %s is not bound to this agent", requestedSourceID)
		}
		sourceIDs = []string{requestedSourceID}
	}
	if len(sourceIDs) == 0 {
		return nil, nil, fmt.Errorf("no database sources are bound to this agent")
	}
	sourceTenantID := scope.SourceTenantID
	if sourceTenantID == 0 {
		sourceTenantID = scope.TenantID
	}
	var ownSources []Source
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id IN ? AND status = ?", sourceTenantID, sourceIDs, SourceStatusActive).Find(&ownSources).Error; err != nil {
		return nil, nil, err
	}
	sourceByID := make(map[string]Source, len(sourceIDs))
	for _, src := range ownSources {
		sourceByID[src.ID] = src
	}
	if len(sourceByID) < len(sourceIDs) {
		remaining := make([]string, 0, len(sourceIDs)-len(sourceByID))
		for _, id := range sourceIDs {
			if _, ok := sourceByID[id]; !ok {
				remaining = append(remaining, id)
			}
		}
		if len(remaining) > 0 {
			var members []types.OrganizationTenantMember
			if err := s.db.WithContext(ctx).Where("tenant_id = ?", sourceTenantID).Find(&members).Error; err != nil {
				return nil, nil, err
			}
			memberByOrg := make(map[string]types.OrganizationTenantMember, len(members))
			orgIDs := make([]string, 0, len(members))
			for _, member := range members {
				memberByOrg[member.OrganizationID] = member
				orgIDs = append(orgIDs, member.OrganizationID)
			}
			if len(orgIDs) > 0 {
				var shares []SourceShare
				if err := s.db.WithContext(ctx).
					Preload("Source").
					Where("source_id IN ? AND organization_id IN ?", remaining, orgIDs).
					Find(&shares).Error; err != nil {
					return nil, nil, err
				}
				for _, share := range shares {
					if share.Source == nil || share.Source.Status != SourceStatusActive {
						continue
					}
					member := memberByOrg[share.OrganizationID]
					effective := effectiveSourceSharePermission(share.Permission, member.Role, scope.TenantRole)
					if !effective.HasPermission(types.OrgRoleViewer) {
						continue
					}
					sourceByID[share.Source.ID] = *share.Source
				}
			}
		}
	}
	if len(sourceByID) == 0 {
		return nil, nil, fmt.Errorf("no active database sources are available")
	}
	ids := make([]string, 0, len(sourceByID))
	sourceTenantIDs := make([]uint64, 0, len(sourceByID))
	seenSourceTenantID := make(map[uint64]bool, len(sourceByID))
	for _, src := range sourceByID {
		ids = append(ids, src.ID)
		if src.TenantID != 0 && !seenSourceTenantID[src.TenantID] {
			seenSourceTenantID[src.TenantID] = true
			sourceTenantIDs = append(sourceTenantIDs, src.TenantID)
		}
	}
	var tables []SourceTable
	query := s.db.WithContext(ctx).
		Preload("Columns", func(db *gorm.DB) *gorm.DB { return db.Order("ordinal ASC") }).
		Where("source_id IN ? AND enabled = ?", ids, true)
	if len(sourceTenantIDs) > 0 {
		query = query.Where("tenant_id IN ?", sourceTenantIDs)
	}
	if err := query.Order("schema_name ASC, table_name ASC").Find(&tables).Error; err != nil {
		return nil, nil, err
	}
	return sourceByID, tables, nil
}

func sampleValuesForColumn(rows []map[string]any, col string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 5)
	for _, row := range rows {
		v, ok := row[col]
		if !ok || v == nil {
			continue
		}
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= 5 {
			break
		}
	}
	return out
}

func parseStringArray(raw types.JSON) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

func isHiddenColumn(col SourceColumn) bool {
	return strings.EqualFold(strings.TrimSpace(col.SensitiveLevel), "hidden")
}

func virtualTableName(src *Source, schema, table string) string {
	base := sourceSQLPrefix(src) + "__" + sanitizeIdent(schema) + "__" + sanitizeIdent(table)
	suffix := "__" + shortIdentHash(src, schema, table)
	return trimIdentWithSuffix(base, suffix)
}

func shortIdentHash(src *Source, schema, table string) string {
	parts := []string{"", "", schema, table}
	if src != nil {
		parts[0] = src.Type
		parts[1] = src.ID
	}
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", sum[:4])
}

func sourceSQLPrefix(src *Source) string {
	if src == nil {
		return "db"
	}
	sourceType := sanitizeIdent(src.Type)
	if sourceType == "t" {
		sourceType = "db"
	}
	id := strings.ReplaceAll(src.ID, "-", "")
	if len(id) >= 8 {
		return sourceType + "_" + strings.ToLower(id[:8])
	}
	if name := sanitizeIdent(src.Name); name != "t" {
		return sourceType + "_" + name
	}
	return sourceType
}

var identRe = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

func sanitizeIdent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = identRe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "t"
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "t_" + s
	}
	return s
}

func trimIdent(s string) string {
	if len(s) <= maxVirtualIdentLength {
		return s
	}
	return s[:maxVirtualIdentLength]
}

func trimIdentWithSuffix(base, suffix string) string {
	if len(base)+len(suffix) <= maxVirtualIdentLength {
		return base + suffix
	}
	baseLimit := maxVirtualIdentLength - len(suffix)
	if baseLimit < 1 {
		return trimIdent(suffix)
	}
	trimmed := strings.Trim(base[:baseLimit], "_")
	if trimmed == "" {
		trimmed = "t"
	}
	return trimmed + suffix
}

func defaultSchema(src *Source) string {
	if src == nil {
		return ""
	}
	cfg, _ := src.ParseConfig()
	if src.Type == SourceTypePostgres {
		return "public"
	}
	return cfg.Database
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func tablesReferencedBySQL(tables []SourceTable, referencedNames []string, sqlTexts ...string) []SourceTable {
	needed := make(map[string]bool, len(referencedNames))
	for _, name := range referencedNames {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			needed[name] = true
		}
	}
	for _, sqlText := range sqlTexts {
		sqlText = strings.ToLower(stripSQLStringLiterals(sqlText))
		if sqlText == "" {
			continue
		}
		for _, table := range tables {
			name := strings.ToLower(strings.TrimSpace(table.VirtualName))
			if name != "" && containsSQLIdentifier(sqlText, name) {
				needed[name] = true
			}
		}
	}
	if len(needed) == 0 {
		return nil
	}
	out := make([]SourceTable, 0, len(needed))
	for _, table := range tables {
		if needed[strings.ToLower(table.VirtualName)] {
			out = append(out, table)
		}
	}
	return out
}

func containsSQLIdentifier(sqlText, ident string) bool {
	if ident == "" {
		return false
	}
	for offset := 0; offset < len(sqlText); {
		idx := strings.Index(sqlText[offset:], ident)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(ident)
		if (start == 0 || !isSQLIdentChar(sqlText[start-1])) && (end == len(sqlText) || !isSQLIdentChar(sqlText[end])) {
			return true
		}
		offset = end
	}
	return false
}

func isSQLIdentChar(ch byte) bool {
	return ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}

func stripSQLStringLiterals(sqlText string) string {
	var b strings.Builder
	b.Grow(len(sqlText))
	inSingle := false
	for i := 0; i < len(sqlText); i++ {
		ch := sqlText[i]
		if inSingle {
			b.WriteByte(' ')
			if ch == '\\' && i+1 < len(sqlText) {
				i++
				b.WriteByte(' ')
				continue
			}
			if ch == '\'' {
				if i+1 < len(sqlText) && sqlText[i+1] == '\'' {
					i++
					b.WriteByte(' ')
					continue
				}
				inSingle = false
			}
			continue
		}
		if ch == '\'' {
			inSingle = true
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	parts := regexp.MustCompile(`[\s,，。.;:：/\\|]+`).Split(s, -1)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len([]rune(part)) >= 2 {
			out = append(out, part)
		}
	}
	return out
}

func scoreTable(table SourceTable, terms []string) int {
	hay := strings.ToLower(table.SchemaName + " " + table.PhysicalName + " " + table.VirtualName + " " + table.Description)
	for _, col := range table.Columns {
		hay += " " + strings.ToLower(col.ColumnName+" "+col.Description+" "+col.SemanticType)
	}
	score := 0
	for _, term := range terms {
		if strings.Contains(hay, term) {
			score += 10
		}
	}
	return score
}

func inferBusinessSemantics(tables []SourceTable) []map[string]any {
	out := make([]map[string]any, 0, len(tables))
	tableByName := make(map[string]SourceTable, len(tables))
	for _, table := range tables {
		tableByName[strings.ToLower(table.PhysicalName)] = table
	}
	for _, table := range tables {
		columns := make([]map[string]any, 0, len(table.Columns))
		metricNames := make([]string, 0)
		dimensionNames := make([]string, 0)
		timeNames := make([]string, 0)
		foreignKeys := make([]map[string]string, 0)
		for _, col := range table.Columns {
			if isHiddenColumn(col) {
				continue
			}
			meaning := inferColumnBusinessMeaning(table.PhysicalName, col)
			columns = append(columns, map[string]any{
				"name":             col.ColumnName,
				"business_meaning": meaning,
				"semantic_type":    col.SemanticType,
			})
			switch col.SemanticType {
			case "metric":
				metricNames = append(metricNames, col.ColumnName)
			case "time":
				timeNames = append(timeNames, col.ColumnName)
			default:
				dimensionNames = append(dimensionNames, col.ColumnName)
			}
			if fk := inferForeignKey(tableByName, col.ColumnName); fk != "" {
				foreignKeys = append(foreignKeys, map[string]string{"column": col.ColumnName, "likely_references": fk})
			}
		}
		out = append(out, map[string]any{
			"sql_table_name":       table.VirtualName,
			"business_meaning":     inferTableBusinessMeaning(table),
			"grain_hint":           inferTableGrain(table),
			"metric_columns":       metricNames,
			"dimension_columns":    dimensionNames,
			"time_columns":         timeNames,
			"likely_relationships": foreignKeys,
			"columns":              columns,
		})
	}
	return out
}

func inferTableBusinessMeaning(table SourceTable) string {
	if desc := strings.TrimSpace(table.Description); desc != "" {
		return desc
	}
	name := strings.ReplaceAll(table.PhysicalName, "_", " ")
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "order"):
		return "order or transaction records"
	case strings.Contains(lower, "customer") || strings.Contains(lower, "user"):
		return "customer or user dimension records"
	case strings.Contains(lower, "product") || strings.Contains(lower, "sku"):
		return "product or SKU dimension records"
	case strings.Contains(lower, "payment"):
		return "payment records"
	case strings.Contains(lower, "refund"):
		return "refund or after-sales records"
	case strings.Contains(lower, "inventory") || strings.Contains(lower, "stock"):
		return "inventory or stock records"
	case strings.Contains(lower, "campaign") || strings.Contains(lower, "marketing"):
		return "marketing campaign records"
	default:
		return "business table inferred from table and column names: " + name
	}
}

func inferTableGrain(table SourceTable) string {
	lower := strings.ToLower(table.PhysicalName)
	for _, col := range table.Columns {
		colName := strings.ToLower(col.ColumnName)
		if colName == "id" || strings.HasSuffix(colName, "_id") {
			if strings.Contains(lower, "item") || strings.Contains(lower, "line") {
				return "likely one row per detail line"
			}
			if strings.Contains(lower, "daily") || strings.Contains(lower, "day") {
				return "likely one row per day and dimension"
			}
			return "likely one row per " + strings.TrimSuffix(strings.TrimSuffix(lower, "s"), "_id") + " entity/event"
		}
	}
	return "grain should be confirmed from primary keys, names and sample rows"
}

func inferColumnBusinessMeaning(tableName string, col SourceColumn) string {
	if desc := strings.TrimSpace(col.Description); desc != "" {
		return desc
	}
	name := strings.ToLower(strings.ReplaceAll(col.ColumnName, "_", " "))
	switch {
	case name == "id":
		return "primary identifier of the row"
	case strings.HasSuffix(strings.ToLower(col.ColumnName), "_id"):
		return "identifier that may join to a related dimension/entity table"
	case strings.Contains(name, "amount"), strings.Contains(name, "price"), strings.Contains(name, "revenue"), strings.Contains(name, "cost"):
		return "monetary metric"
	case strings.Contains(name, "qty") || strings.Contains(name, "quantity") || strings.Contains(name, "count"):
		return "quantity/count metric"
	case strings.Contains(name, "status"):
		return "status/category dimension"
	case strings.Contains(name, "date") || strings.Contains(name, "time") || strings.HasSuffix(strings.ToLower(col.ColumnName), "_at"):
		return "time dimension"
	case strings.Contains(name, "name") || strings.Contains(name, "type") || strings.Contains(name, "category"):
		return "descriptive/category dimension"
	default:
		return "field in " + tableName + " inferred from name and type"
	}
}

func inferForeignKey(tableByName map[string]SourceTable, columnName string) string {
	lower := strings.ToLower(columnName)
	if !strings.HasSuffix(lower, "_id") || lower == "id" {
		return ""
	}
	stem := strings.TrimSuffix(lower, "_id")
	candidates := []string{stem, stem + "s", stem + "_info", stem + "_dim", "dim_" + stem}
	for _, candidate := range candidates {
		if table, ok := tableByName[candidate]; ok {
			return table.VirtualName
		}
	}
	return ""
}

func sourceSummaries(sources map[string]Source) []map[string]any {
	out := make([]map[string]any, 0, len(sources))
	for _, src := range sources {
		out = append(out, map[string]any{"id": src.ID, "name": src.Name, "type": src.Type, "description": src.Description})
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["name"]) < fmt.Sprint(out[j]["name"]) })
	return out
}

func normalizeAnalysisSQL(sqlText string) string {
	sqlText = strings.TrimSpace(sqlText)
	sqlText = strings.TrimSuffix(sqlText, ";")
	return strings.TrimSpace(sqlText)
}

func withDuckDBSQLHint(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w. SQL is executed by DuckDB after materializing the source tables; use DuckDB-compatible syntax rather than source-database-specific SQL", err)
}

func limitOuterQuery(sqlText string, maxRows int) string {
	if maxRows <= 0 {
		maxRows = 1000
	}
	return fmt.Sprintf("SELECT * FROM (%s) AS __weknora_limited LIMIT %d", sqlText, maxRows)
}

func effectiveMaxRows(sources map[string]Source, tables []SourceTable) int {
	maxRows := 1000
	for _, table := range tables {
		if src, ok := sources[table.SourceID]; ok && src.MaxRows > 0 && src.MaxRows < maxRows {
			maxRows = src.MaxRows
		}
	}
	return maxRows
}

func effectiveMaxScanRows(sources map[string]Source, tables []SourceTable) int {
	maxRows := 50000
	for _, table := range tables {
		if src, ok := sources[table.SourceID]; ok && src.MaxScanRows > 0 && src.MaxScanRows < maxRows {
			maxRows = src.MaxScanRows
		}
	}
	return maxRows
}

func quoteDuckIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func duckType(dataType string) string {
	t := strings.ToLower(dataType)
	switch {
	case strings.Contains(t, "bigint"):
		return "BIGINT"
	case strings.Contains(t, "int"):
		return "INTEGER"
	case strings.Contains(t, "decimal"), strings.Contains(t, "numeric"), strings.Contains(t, "double"), strings.Contains(t, "float"), strings.Contains(t, "real"), strings.Contains(t, "money"):
		return "DOUBLE"
	case strings.Contains(t, "bool"):
		return "BOOLEAN"
	case strings.Contains(t, "date"), strings.Contains(t, "time"):
		return "TIMESTAMP"
	default:
		return "VARCHAR"
	}
}

func scanRowsToMaps(rows *sql.Rows) ([]map[string]any, []map[string]any, error) {
	names, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	typesInfo, _ := rows.ColumnTypes()
	cols := make([]map[string]any, 0, len(names))
	for i, name := range names {
		dataType := "text"
		if i < len(typesInfo) && typesInfo[i].DatabaseTypeName() != "" {
			dataType = typesInfo[i].DatabaseTypeName()
		}
		cols = append(cols, map[string]any{"name": name, "type": dataType, "semantic_type": inferSemanticType(name, dataType)})
	}
	out := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		item := make(map[string]any, len(names))
		for i, name := range names {
			item[name] = normalizeDBValue(values[i])
		}
		out = append(out, item)
	}
	return cols, out, rows.Err()
}

type chartColumnGroups struct {
	dimensions []string
	metrics    []string
	times      []string
}

type chartIntentHints struct {
	intent          string
	primaryMetric   string
	secondaryMetric string
	dimension       string
	series          string
	title           string
}

func chartHintsFromInput(input QueryInput) chartIntentHints {
	return chartIntentHints{
		intent:          strings.TrimSpace(input.ChartIntent),
		primaryMetric:   strings.TrimSpace(input.PrimaryMetric),
		secondaryMetric: strings.TrimSpace(input.SecondaryMetric),
		dimension:       strings.TrimSpace(input.Dimension),
		series:          strings.TrimSpace(input.Series),
		title:           strings.TrimSpace(input.ChartTitle),
	}
}

func inferChartSpec(columns []map[string]any, rows []map[string]any, preferred string, requested bool, hints chartIntentHints) map[string]any {
	spec := map[string]any{
		"eligible":       false,
		"id":             "",
		"type":           "",
		"default_type":   "",
		"x":              "",
		"y":              []string{},
		"group":          "",
		"secondary_y":    []string{},
		"value":          "",
		"reason":         "chart not requested",
		"language":       "zh-CN",
		"labels_locale":  "zh-CN",
		"table_visible":  false,
		"explicit_chart": requested,
		"chart_intent":   hints.intent,
		"chart_title":    hints.title,
		"contract":       map[string]any{},
		"validation":     map[string]any{"status": "not_requested", "issues": []string{"chart not requested"}},
	}
	if !requested {
		return spec
	}
	if len(rows) == 0 || len(columns) < 2 {
		spec["reason"] = "not enough result data for chart"
		spec["validation"] = map[string]any{"status": "invalid", "issues": []string{"not enough result data for chart"}}
		return spec
	}
	groups := classifyChartColumns(columns, rows)
	if len(groups.metrics) == 0 {
		spec["reason"] = "no numeric metric column"
		spec["validation"] = map[string]any{"status": "invalid", "issues": []string{"no numeric metric column"}}
		return spec
	}
	chartType := normalizePreferredChart(preferred)
	if chartType == "" {
		chartType = chooseDefaultChartType(groups)
	}
	if !supportedStructuredChartType(chartType) {
		spec["reason"] = "unsupported chart type"
		spec["validation"] = map[string]any{"status": "invalid", "issues": []string{"unsupported chart type"}}
		return spec
	}

	if !populateChartSpec(spec, chartType, groups, rows, hints) {
		if spec["reason"] == "" {
			spec["reason"] = "result shape is not suitable for requested chart"
		}
		spec["validation"] = map[string]any{"status": "invalid", "issues": []string{fmt.Sprint(spec["reason"])}}
		return spec
	}

	spec["eligible"] = true
	spec["type"] = chartType
	spec["default_type"] = chartType
	spec["id"] = structuredChartID(chartType, spec)
	spec["reason"] = ""
	contract := buildChartContract(spec, chartType, groups, columns, hints)
	spec["contract"] = contract
	spec["validation"] = validateChartContract(contract, columns)
	return spec
}

func classifyChartColumns(columns []map[string]any, rows []map[string]any) chartColumnGroups {
	var groups chartColumnGroups
	for _, col := range columns {
		name := fmt.Sprint(col["name"])
		sem := fmt.Sprint(col["semantic_type"])
		if sem == "metric" && columnLooksNumeric(rows, name) {
			groups.metrics = append(groups.metrics, name)
		} else if sem == "time" {
			groups.times = append(groups.times, name)
		} else {
			groups.dimensions = append(groups.dimensions, name)
		}
	}
	return groups
}

func normalizePreferredChart(preferred string) string {
	chartType := strings.ToLower(strings.TrimSpace(preferred))
	chartType = strings.ReplaceAll(chartType, "-", "_")
	chartType = strings.ReplaceAll(chartType, " ", "_")
	switch chartType {
	case "combo", "dual_axis", "dual_axis_chart", "dual_axis_bar_line", "bar_line", "bar_line_combo":
		return "dual_axis_combo"
	case "stacked", "stackedbar", "stacked_bar_chart":
		return "stacked_bar"
	case "tree_map":
		return "treemap"
	default:
		return chartType
	}
}

func supportedStructuredChartType(chartType string) bool {
	switch chartType {
	case "line", "bar", "stacked_bar", "pie", "scatter", "histogram", "heatmap", "funnel", "dual_axis_combo",
		"area", "radar", "treemap", "boxplot":
		return true
	default:
		return false
	}
}

func chooseDefaultChartType(groups chartColumnGroups) string {
	if len(groups.times) > 0 {
		if len(groups.metrics) >= 2 {
			return "dual_axis_combo"
		}
		return "line"
	}
	if len(groups.dimensions) >= 2 && len(groups.metrics) > 0 {
		return "stacked_bar"
	}
	if len(groups.metrics) >= 2 && len(groups.dimensions) == 0 {
		return "scatter"
	}
	return "bar"
}

func chartDimensionCandidates(groups chartColumnGroups, preferTime bool) []string {
	out := make([]string, 0, len(groups.dimensions)+len(groups.times))
	add := func(items []string) {
		for _, item := range items {
			if item != "" && !containsString(out, item) {
				out = append(out, item)
			}
		}
	}
	if preferTime {
		add(groups.times)
		add(groups.dimensions)
	} else {
		add(groups.dimensions)
		add(groups.times)
	}
	return out
}

func preferredChartField(preferred string, candidates []string) string {
	preferred = strings.TrimSpace(preferred)
	if preferred == "" {
		return ""
	}
	for _, field := range candidates {
		if strings.TrimSpace(field) == preferred {
			return field
		}
	}
	for _, field := range candidates {
		if strings.EqualFold(strings.TrimSpace(field), preferred) {
			return field
		}
	}
	return ""
}

func chooseChartDimension(groups chartColumnGroups, preferred string, preferTime bool, exclude ...string) string {
	candidates := chartDimensionCandidates(groups, preferTime)
	filtered := make([]string, 0, len(candidates))
	for _, field := range candidates {
		if field == "" || containsString(exclude, field) {
			continue
		}
		filtered = append(filtered, field)
	}
	if field := preferredChartField(preferred, filtered); field != "" {
		return field
	}
	return firstString(filtered)
}

func chooseChartMetric(groups chartColumnGroups, preferred string, exclude ...string) string {
	candidates := make([]string, 0, len(groups.metrics))
	for _, field := range groups.metrics {
		if field == "" || containsString(exclude, field) {
			continue
		}
		candidates = append(candidates, field)
	}
	if field := preferredChartField(preferred, candidates); field != "" {
		return field
	}
	return firstString(candidates)
}

func orderedChartMetrics(groups chartColumnGroups, preferred ...string) []string {
	out := make([]string, 0, len(groups.metrics))
	for _, hint := range preferred {
		if field := preferredChartField(hint, groups.metrics); field != "" && !containsString(out, field) {
			out = append(out, field)
		}
	}
	for _, field := range groups.metrics {
		if field != "" && !containsString(out, field) {
			out = append(out, field)
		}
	}
	return out
}

func populateChartSpec(spec map[string]any, chartType string, groups chartColumnGroups, rows []map[string]any, hints chartIntentHints) bool {
	dimensionAxis := chooseChartDimension(groups, hints.dimension, true)
	categoryAxis := chooseChartDimension(groups, hints.dimension, false)
	metric := chooseChartMetric(groups, hints.primaryMetric)

	switch chartType {
	case "line", "area":
		if dimensionAxis == "" || metric == "" {
			spec["reason"] = "line/area chart requires one dimension/time column and one metric column"
			return false
		}
		spec["x"] = dimensionAxis
		if preferredChartField(hints.primaryMetric, groups.metrics) != "" {
			spec["y"] = []string{metric}
		} else {
			spec["y"] = groups.metrics
		}
	case "bar":
		if categoryAxis == "" || metric == "" {
			spec["reason"] = "bar chart requires one category column and one metric column"
			return false
		}
		spec["x"] = categoryAxis
		if preferredChartField(hints.primaryMetric, groups.metrics) != "" {
			spec["y"] = []string{metric}
		} else {
			spec["y"] = groups.metrics
		}
	case "stacked_bar":
		if categoryAxis == "" || len(groups.dimensions)+len(groups.times) < 2 || metric == "" {
			spec["reason"] = "stacked bar chart requires two dimensions and one metric column"
			return false
		}
		spec["x"] = categoryAxis
		spec["group"] = chooseChartDimension(groups, hints.series, false, categoryAxis)
		spec["y"] = []string{metric}
	case "pie", "funnel", "treemap":
		if categoryAxis == "" || metric == "" {
			spec["reason"] = chartType + " requires one category column and one metric column"
			return false
		}
		spec["x"] = categoryAxis
		spec["y"] = []string{metric}
		if chartType == "treemap" {
			spec["group"] = chooseChartDimension(groups, hints.series, false, categoryAxis)
		}
	case "scatter":
		if len(groups.metrics) < 2 {
			spec["reason"] = "scatter chart requires at least two metric columns"
			return false
		}
		xMetric := chooseChartMetric(groups, hints.primaryMetric)
		yMetric := chooseChartMetric(groups, hints.secondaryMetric, xMetric)
		if xMetric == "" || yMetric == "" {
			spec["reason"] = "scatter chart requires two distinct metric columns"
			return false
		}
		spec["x"] = xMetric
		spec["y"] = []string{yMetric}
	case "histogram":
		if metric == "" {
			spec["reason"] = "histogram requires one numeric metric column"
			return false
		}
		spec["x"] = metric
		if len(groups.metrics) > 1 {
			spec["y"] = []string{groups.metrics[1]}
		}
	case "heatmap":
		if categoryAxis == "" || len(groups.dimensions)+len(groups.times) < 2 || metric == "" {
			spec["reason"] = "heatmap requires two dimensions and one metric column"
			return false
		}
		spec["x"] = categoryAxis
		spec["group"] = chooseChartDimension(groups, hints.series, false, categoryAxis)
		spec["y"] = []string{metric}
	case "dual_axis_combo":
		if dimensionAxis == "" || len(groups.metrics) < 2 {
			spec["reason"] = "dual-axis combo chart requires one dimension and two metric columns"
			return false
		}
		primary := chooseChartMetric(groups, hints.primaryMetric)
		secondary := chooseChartMetric(groups, hints.secondaryMetric, primary)
		if primary == "" || secondary == "" {
			spec["reason"] = "dual-axis combo chart requires two distinct metric columns"
			return false
		}
		spec["x"] = dimensionAxis
		spec["y"] = []string{primary}
		spec["secondary_y"] = []string{secondary}
	case "radar":
		if len(groups.metrics) < 3 {
			spec["reason"] = "radar chart requires at least three metric columns"
			return false
		}
		spec["x"] = categoryAxis
		spec["y"] = orderedChartMetrics(groups, hints.primaryMetric, hints.secondaryMetric)
	case "boxplot":
		fiveNumber := inferFiveNumberFields(groups.metrics)
		if len(fiveNumber) == 5 {
			spec["x"] = categoryAxis
			spec["y"] = fiveNumber
			break
		}
		if metric == "" {
			spec["reason"] = "boxplot requires raw numeric values or min/q1/median/q3/max columns"
			return false
		}
		spec["x"] = categoryAxis
		spec["y"] = []string{metric}
	default:
		return false
	}
	return true
}

func buildChartContract(spec map[string]any, chartType string, groups chartColumnGroups, columns []map[string]any, hints chartIntentHints) map[string]any {
	x := stringFromAny(spec["x"])
	yFields := stringsFromAny(spec["y"])
	metric := firstString(yFields)
	group := stringFromAny(spec["group"])
	secondary := firstString(stringsFromAny(spec["secondary_y"]))
	value := stringFromAny(spec["value"])
	if value == "" {
		value = metric
	}
	spec["value"] = value

	encoding := map[string]any{}
	setEncodingField(encoding, "x", x, chartFieldRole(groups, x), "")
	switch chartType {
	case "heatmap":
		setEncodingField(encoding, "y", group, chartFieldRole(groups, group), "")
		setEncodingField(encoding, "value", value, "metric", "sum")
	case "stacked_bar":
		setEncodingField(encoding, "series", group, chartFieldRole(groups, group), "")
		setEncodingField(encoding, "stack", group, chartFieldRole(groups, group), "")
		setEncodingField(encoding, "value", value, "metric", "sum")
	case "scatter":
		setEncodingField(encoding, "y", metric, "metric", "")
		if x != "" {
			setEncodingField(encoding, "x", x, "metric", "")
		}
	case "histogram":
		setEncodingField(encoding, "value", x, "metric", "count")
	case "dual_axis_combo":
		setEncodingField(encoding, "value", metric, "metric", "sum")
		setEncodingField(encoding, "secondary_value", secondary, "metric", "sum")
	case "treemap":
		hierarchy := []map[string]any{}
		if x != "" {
			hierarchy = append(hierarchy, chartEncodingField(x, chartFieldRole(groups, x), ""))
		}
		if group != "" {
			hierarchy = append(hierarchy, chartEncodingField(group, chartFieldRole(groups, group), ""))
		}
		encoding["hierarchy"] = hierarchy
		setEncodingField(encoding, "value", value, "metric", "sum")
	case "radar", "boxplot":
		setEncodingField(encoding, "value", metric, "metric", chartAggregateForType(chartType))
		setEncodingFields(encoding, "values", yFields, "metric", chartAggregateForType(chartType))
	default:
		setEncodingField(encoding, "y", metric, "metric", "")
		setEncodingField(encoding, "value", value, "metric", "sum")
		if len(yFields) > 1 {
			setEncodingFields(encoding, "values", yFields, "metric", "sum")
		}
	}

	groupBy := chartGroupBy(chartType, x, group)
	availableFields := columnNamesFromMaps(columns)
	visualDimensions := chartVisualDimensions(chartType, x, group)
	visualMetrics := chartVisualMetrics(chartType, x, yFields, secondary, value)
	visualFields := uniqueNonEmptyStrings(append(visualDimensions, visualMetrics...)...)
	contract := map[string]any{
		"id":       stringFromAny(spec["id"]),
		"type":     chartType,
		"intent":   map[string]any{"task": chartTask(chartType), "reason": hints.intent},
		"encoding": encoding,
		"transform": map[string]any{
			"group_by":      groupBy,
			"aggregate":     chartAggregateForType(chartType),
			"dedupe_policy": chartDedupePolicy(chartType),
			"sort":          []map[string]any{},
			"limit":         nil,
		},
		"visual_scope": map[string]any{
			"dimensions":  visualDimensions,
			"series":      group,
			"metrics":     visualMetrics,
			"fields":      visualFields,
			"description": "fields actually encoded by the rendered chart",
		},
		"evidence_scope": map[string]any{
			"available_fields":  availableFields,
			"visualized_fields": visualFields,
			"non_visual_fields": differenceStrings(availableFields, visualFields),
			"description":       "query result fields may support textual insights; do not claim non_visual_fields are visually encoded by this chart",
		},
		"display": map[string]any{
			"title":         chartDisplayTitle(chartType, x, group, value, hints.title),
			"x_label":       x,
			"y_label":       chartYLabel(chartType, group, value),
			"value_label":   value,
			"legend_title":  chartLegendTitle(chartType, group),
			"language":      "zh-CN",
			"table_visible": false,
		},
		"metadata": map[string]any{
			"source":  "db_query",
			"columns": columnNamesFromMaps(columns),
		},
	}
	return contract
}

func uniqueNonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || containsString(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func differenceStrings(all []string, used []string) []string {
	out := make([]string, 0, len(all))
	for _, value := range all {
		value = strings.TrimSpace(value)
		if value == "" || containsString(used, value) || containsString(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func chartVisualDimensions(chartType, x, group string) []string {
	switch chartType {
	case "scatter", "histogram":
		return []string{}
	case "heatmap", "stacked_bar", "treemap":
		return uniqueNonEmptyStrings(x, group)
	default:
		return uniqueNonEmptyStrings(x)
	}
}

func chartVisualMetrics(chartType, x string, yFields []string, secondary, value string) []string {
	switch chartType {
	case "scatter":
		return uniqueNonEmptyStrings(x, firstString(yFields))
	case "histogram":
		return uniqueNonEmptyStrings(x)
	case "dual_axis_combo":
		return uniqueNonEmptyStrings(value, secondary)
	default:
		return uniqueNonEmptyStrings(append(yFields, value)...)
	}
}

func chartEncodingField(field, role, aggregate string) map[string]any {
	out := map[string]any{"field": field, "role": role}
	if aggregate != "" {
		out["aggregate"] = aggregate
	}
	return out
}

func setEncodingField(encoding map[string]any, key, field, role, aggregate string) {
	if strings.TrimSpace(field) == "" {
		encoding[key] = map[string]any{"field": "", "role": role}
		return
	}
	encoding[key] = chartEncodingField(field, role, aggregate)
}

func setEncodingFields(encoding map[string]any, key string, fields []string, role, aggregate string) {
	items := make([]map[string]any, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		items = append(items, chartEncodingField(field, role, aggregate))
	}
	if len(items) > 0 {
		encoding[key] = items
	}
}

func chartFieldRole(groups chartColumnGroups, field string) string {
	if field == "" {
		return ""
	}
	for _, item := range groups.metrics {
		if item == field {
			return "metric"
		}
	}
	for _, item := range groups.times {
		if item == field {
			return "time"
		}
	}
	for _, item := range groups.dimensions {
		if item == field {
			return "dimension"
		}
	}
	return "dimension"
}

func chartTask(chartType string) string {
	switch chartType {
	case "line", "area", "dual_axis_combo":
		return "trend"
	case "pie", "stacked_bar", "treemap":
		return "composition"
	case "histogram", "boxplot":
		return "distribution"
	case "scatter":
		return "relationship"
	case "heatmap":
		return "comparison"
	case "funnel":
		return "conversion"
	default:
		return "comparison"
	}
}

func chartAggregateForType(chartType string) string {
	switch chartType {
	case "histogram":
		return "count"
	case "scatter", "boxplot":
		return "none"
	default:
		return "sum"
	}
}

func chartDedupePolicy(chartType string) string {
	switch chartType {
	case "scatter", "boxplot":
		return "keep"
	default:
		return "aggregate"
	}
}

func chartGroupBy(chartType, x, group string) []string {
	out := make([]string, 0, 3)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || containsString(out, value) {
			return
		}
		out = append(out, value)
	}
	switch chartType {
	case "heatmap", "stacked_bar":
		add(x)
		add(group)
	default:
		add(x)
	}
	return out
}

func chartDisplayTitle(chartType, x, group, value, preferred string) string {
	preferred = strings.TrimSpace(preferred)
	if preferred != "" {
		return preferred
	}
	if x != "" && group != "" && value != "" {
		switch chartType {
		case "heatmap":
			return fmt.Sprintf("%s与%s%s热力图", x, group, value)
		case "stacked_bar":
			return fmt.Sprintf("%s-%s%s构成", x, group, value)
		case "treemap":
			return fmt.Sprintf("%s-%s%s树图", x, group, value)
		}
	}
	if x != "" && value != "" {
		switch chartType {
		case "line":
			return fmt.Sprintf("%s%s趋势", x, value)
		case "area":
			return fmt.Sprintf("%s%s面积趋势", x, value)
		case "bar":
			return fmt.Sprintf("%s%s对比", x, value)
		case "pie":
			return fmt.Sprintf("%s%s占比", x, value)
		case "funnel":
			return fmt.Sprintf("%s%s漏斗", x, value)
		case "dual_axis_combo":
			return fmt.Sprintf("%s多指标趋势", x)
		}
	}
	switch chartType {
	case "heatmap":
		return "交叉热力分析"
	case "stacked_bar":
		return "分组堆叠对比"
	case "line":
		return "趋势分析"
	case "area":
		return "面积趋势分析"
	case "pie":
		return "占比分析"
	case "scatter":
		return "散点关系分析"
	case "histogram":
		return "分布分析"
	case "funnel":
		return "漏斗分析"
	case "dual_axis_combo":
		return "双轴组合分析"
	case "radar":
		return "雷达对比分析"
	case "treemap":
		return "层级树图"
	case "boxplot":
		return "箱线分布分析"
	default:
		if x != "" && value != "" {
			return fmt.Sprintf("%s对比", x)
		}
		if group != "" {
			return fmt.Sprintf("%s分析", group)
		}
		return "数据图表"
	}
}

func chartYLabel(chartType, group, value string) string {
	switch chartType {
	case "heatmap":
		return group
	case "scatter", "boxplot":
		return value
	default:
		if value != "" {
			return value
		}
		return "数值"
	}
}

func chartLegendTitle(chartType, group string) string {
	switch chartType {
	case "stacked_bar", "heatmap":
		return group
	default:
		return ""
	}
}

func validateChartContract(contract map[string]any, columns []map[string]any) map[string]any {
	issues := make([]string, 0)
	chartType := stringFromAny(contract["type"])
	encoding, _ := contract["encoding"].(map[string]any)
	columnSet := make(map[string]bool, len(columns))
	for _, col := range columns {
		name := strings.TrimSpace(fmt.Sprint(col["name"]))
		if name != "" {
			columnSet[name] = true
		}
	}
	requireField := func(key string) string {
		field := encodingFieldName(encoding, key)
		if field == "" {
			issues = append(issues, fmt.Sprintf("encoding.%s is required", key))
			return ""
		}
		if !columnSet[field] {
			issues = append(issues, fmt.Sprintf("encoding.%s field %q is not in query columns", key, field))
		}
		return field
	}
	requireAnyHierarchy := func() {
		raw, _ := encoding["hierarchy"].([]map[string]any)
		if len(raw) == 0 {
			if arr, ok := encoding["hierarchy"].([]any); ok {
				if len(arr) == 0 {
					issues = append(issues, "encoding.hierarchy is required")
				}
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						field := stringFromAny(m["field"])
						if field == "" {
							issues = append(issues, "encoding.hierarchy contains an empty field")
						} else if !columnSet[field] {
							issues = append(issues, fmt.Sprintf("encoding.hierarchy field %q is not in query columns", field))
						}
					}
				}
				return
			}
			issues = append(issues, "encoding.hierarchy is required")
			return
		}
		for _, item := range raw {
			field := stringFromAny(item["field"])
			if field == "" {
				issues = append(issues, "encoding.hierarchy contains an empty field")
			} else if !columnSet[field] {
				issues = append(issues, fmt.Sprintf("encoding.hierarchy field %q is not in query columns", field))
			}
		}
	}
	requireFields := func(key string, min int) []string {
		fields := encodingFieldNames(encoding, key)
		if len(fields) < min {
			issues = append(issues, fmt.Sprintf("encoding.%s requires at least %d field(s)", key, min))
			return fields
		}
		for _, field := range fields {
			if !columnSet[field] {
				issues = append(issues, fmt.Sprintf("encoding.%s field %q is not in query columns", key, field))
			}
		}
		return fields
	}

	switch chartType {
	case "heatmap":
		requireField("x")
		requireField("y")
		requireField("value")
	case "stacked_bar":
		requireField("x")
		requireField("series")
		requireField("value")
	case "bar", "line", "area", "pie", "funnel":
		requireField("x")
		requireField("value")
	case "dual_axis_combo":
		requireField("x")
		requireField("value")
		requireField("secondary_value")
	case "scatter":
		requireField("x")
		requireField("y")
	case "histogram":
		requireField("value")
	case "treemap":
		requireAnyHierarchy()
		requireField("value")
	case "radar":
		requireFields("values", 3)
	case "boxplot":
		if len(encodingFieldNames(encoding, "values")) > 0 {
			requireFields("values", 1)
		} else {
			requireField("value")
		}
	default:
		issues = append(issues, "unsupported chart type in contract")
	}

	status := "pass"
	if len(issues) > 0 {
		status = "invalid"
	}
	return map[string]any{"status": status, "issues": issues}
}

func encodingFieldName(encoding map[string]any, key string) string {
	raw, ok := encoding[key]
	if !ok {
		return ""
	}
	switch value := raw.(type) {
	case map[string]any:
		return stringFromAny(value["field"])
	case map[string]string:
		return value["field"]
	default:
		return ""
	}
}

func encodingFieldNames(encoding map[string]any, key string) []string {
	raw, ok := encoding[key]
	if !ok {
		return nil
	}
	switch value := raw.(type) {
	case []map[string]any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if field := stringFromAny(item["field"]); field != "" {
				out = append(out, field)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if m, ok := item.(map[string]any); ok {
				if field := stringFromAny(m["field"]); field != "" {
					out = append(out, field)
				}
			}
		}
		return out
	default:
		if field := encodingFieldName(encoding, key); field != "" {
			return []string{field}
		}
		return nil
	}
}

func columnNamesFromMaps(columns []map[string]any) []string {
	out := make([]string, 0, len(columns))
	for _, col := range columns {
		name := strings.TrimSpace(fmt.Sprint(col["name"]))
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func stringFromAny(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func stringsFromAny(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := stringFromAny(item); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		if text := stringFromAny(value); text != "" && text != "<nil>" {
			return []string{text}
		}
		return nil
	}
}

func firstString(groups ...[]string) string {
	for _, group := range groups {
		if len(group) > 0 {
			return group[0]
		}
	}
	return ""
}

func secondString(dimensions []string, times []string, exclude string) string {
	for _, item := range append(append([]string{}, dimensions...), times...) {
		if item != "" && item != exclude {
			return item
		}
	}
	return ""
}

func findFieldByName(fields []string, aliases ...string) string {
	for _, alias := range aliases {
		needle := strings.ToLower(alias)
		for _, field := range fields {
			lower := strings.ToLower(field)
			if lower == needle || strings.Contains(lower, needle) {
				return field
			}
		}
	}
	return ""
}

func inferFiveNumberFields(metrics []string) []string {
	minField := findFieldByName(metrics, "min", "minimum", "最小")
	q1Field := findFieldByName(metrics, "q1", "p25", "quantile_25", "lower_quartile", "第一四分位")
	medianField := findFieldByName(metrics, "median", "p50", "quantile_50", "中位")
	q3Field := findFieldByName(metrics, "q3", "p75", "quantile_75", "upper_quartile", "第三四分位")
	maxField := findFieldByName(metrics, "max", "maximum", "最大")
	if minField == "" || q1Field == "" || medianField == "" || q3Field == "" || maxField == "" {
		return nil
	}
	return []string{minField, q1Field, medianField, q3Field, maxField}
}

func structuredChartID(chartType string, spec map[string]any) string {
	payload := fmt.Sprintf("%s|%v|%v|%v|%v",
		chartType,
		spec["x"],
		spec["y"],
		spec["group"],
		spec["secondary_y"],
	)
	sum := sha1.Sum([]byte(payload))
	return fmt.Sprintf("chart_%x", sum[:5])
}

func columnLooksNumeric(rows []map[string]any, name string) bool {
	for _, row := range rows {
		v := row[name]
		if v == nil {
			continue
		}
		switch v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return true
		default:
			if _, err := fmt.Sscan(fmt.Sprint(v), new(float64)); err == nil {
				return true
			}
			return false
		}
	}
	return false
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	msg = regexp.MustCompile(`(?i)(password=|passwd=|pwd=)[^\\s&;]+`).ReplaceAllString(msg, "$1***")
	return msg
}

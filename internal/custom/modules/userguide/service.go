package userguide

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

const (
	ResourceName = "使用指南"
)

const (
	guideDescription = "系统使用指南文档。"
	spaceDescription = "系统使用指南知识库共享空间。"
)

type Service struct {
	db             *gorm.DB
	orgService     interfaces.OrganizationService
	kbService      interfaces.KnowledgeBaseService
	kbShareService interfaces.KBShareService
	mu             sync.Mutex
}

type provisionCore struct {
	org   *types.Organization
	kb    *types.KnowledgeBase
	owner *types.User
}

func NewService(
	db *gorm.DB,
	orgService interfaces.OrganizationService,
	kbService interfaces.KnowledgeBaseService,
	kbShareService interfaces.KBShareService,
) *Service {
	return &Service{
		db:             db,
		orgService:     orgService,
		kbService:      kbService,
		kbShareService: kbShareService,
	}
}

// EnsureAllUsers backfills existing active users into the guide sharing space.
func (s *Service) EnsureAllUsers(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	users, err := s.activeUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}

	core, err := s.ensureCoreLocked(ctx, nil)
	if err != nil || core == nil {
		return err
	}

	seenTenants := make(map[uint64]bool, len(users))
	for i := range users {
		user := &users[i]
		if !usableUser(user) || seenTenants[user.TenantID] {
			continue
		}
		seenTenants[user.TenantID] = true
		role := types.OrgRoleViewer
		if user.TenantID == core.owner.TenantID {
			role = types.OrgRoleAdmin
		}
		if err := s.ensureTenantMemberLocked(ctx, core.org.ID, user.TenantID, user.ID, role); err != nil {
			return fmt.Errorf("ensure guide space member for tenant %d: %w", user.TenantID, err)
		}
	}

	return nil
}

// EnsureUserProvisioned makes the guide KB visible to one user's home tenant.
func (s *Service) EnsureUserProvisioned(ctx context.Context, user *types.User) error {
	if s == nil || s.db == nil || !usableUser(user) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	core, err := s.ensureCoreLocked(ctx, user)
	if err != nil || core == nil {
		return err
	}

	role := types.OrgRoleViewer
	if user.TenantID == core.owner.TenantID {
		role = types.OrgRoleAdmin
	}
	return s.ensureTenantMemberLocked(ctx, core.org.ID, user.TenantID, user.ID, role)
}

func (s *Service) ensureCoreLocked(ctx context.Context, preferred *types.User) (*provisionCore, error) {
	org, err := s.findGuideOrg(ctx)
	if err != nil {
		return nil, err
	}

	owner, err := s.resolveOwnerUser(ctx, org, preferred)
	if err != nil || owner == nil {
		return nil, err
	}

	if org == nil {
		org, err = s.createGuideOrg(ctx, owner)
		if err != nil {
			return nil, err
		}
	} else if err := s.ensureOrgUnlimitedLocked(ctx, org); err != nil {
		return nil, err
	}

	if err := s.ensureSourceTenantAdminLocked(ctx, org.ID, owner); err != nil {
		return nil, err
	}

	kb, err := s.findGuideKnowledgeBase(ctx, owner.TenantID)
	if err != nil {
		return nil, err
	}
	if kb == nil {
		kb, err = s.createGuideKnowledgeBase(ctx, owner)
		if err != nil {
			return nil, err
		}
	}

	if err := s.ensureGuideShareLocked(ctx, kb, org, owner); err != nil {
		return nil, err
	}

	return &provisionCore{org: org, kb: kb, owner: owner}, nil
}

func (s *Service) createGuideOrg(ctx context.Context, owner *types.User) (*types.Organization, error) {
	limit := 0
	org, err := s.orgService.CreateOrganization(ctx, owner.ID, owner.TenantID, &types.CreateOrganizationRequest{
		Name:        ResourceName,
		Description: spaceDescription,
		MemberLimit: &limit,
	})
	if err != nil {
		return nil, err
	}
	logger.Infof(ctx, "[custom userguide] created guide space %s for tenant %d", org.ID, owner.TenantID)
	return org, nil
}

func (s *Service) createGuideKnowledgeBase(ctx context.Context, owner *types.User) (*types.KnowledgeBase, error) {
	kbCtx := s.tenantContext(ctx, owner)
	kb, err := s.kbService.CreateKnowledgeBase(kbCtx, &types.KnowledgeBase{
		Name:        ResourceName,
		Description: guideDescription,
		Type:        types.KnowledgeBaseTypeDocument,
	})
	if err != nil {
		return nil, err
	}
	logger.Infof(ctx, "[custom userguide] created guide KB %s for tenant %d", kb.ID, owner.TenantID)
	return kb, nil
}

func (s *Service) ensureGuideShareLocked(ctx context.Context, kb *types.KnowledgeBase, org *types.Organization, owner *types.User) error {
	if kb == nil || org == nil || owner == nil {
		return nil
	}
	if _, err := s.kbShareService.GetShareByKBAndOrg(ctx, kb.ID, org.ID); err == nil {
		return nil
	}
	if _, err := s.kbShareService.ShareKnowledgeBase(ctx, kb.ID, org.ID, owner.ID, owner.TenantID, types.OrgRoleViewer); err != nil {
		if isExistingRelationError(err) {
			return nil
		}
		if existing, lookupErr := s.kbShareService.GetShareByKBAndOrg(ctx, kb.ID, org.ID); lookupErr == nil && existing != nil {
			return nil
		}
		return err
	}
	logger.Infof(ctx, "[custom userguide] shared guide KB %s to space %s", kb.ID, org.ID)
	return nil
}

func (s *Service) ensureTenantMemberLocked(ctx context.Context, orgID string, tenantID uint64, representativeUserID string, role types.OrgMemberRole) error {
	if orgID == "" || tenantID == 0 {
		return nil
	}
	if _, err := s.orgService.GetTenantMember(ctx, orgID, tenantID); err == nil {
		return nil
	}

	err := s.orgService.AddTenantMember(ctx, orgID, tenantID, representativeUserID, role)
	if err == nil || isExistingRelationError(err) {
		return nil
	}
	if strings.Contains(err.Error(), "member limit") {
		if updateErr := s.setOrgMemberLimit(ctx, orgID, 0); updateErr != nil {
			return updateErr
		}
		err = s.orgService.AddTenantMember(ctx, orgID, tenantID, representativeUserID, role)
		if err == nil || isExistingRelationError(err) {
			return nil
		}
	}
	if exists, lookupErr := s.orgService.GetTenantMember(ctx, orgID, tenantID); lookupErr == nil && exists != nil {
		return nil
	}
	return err
}

func (s *Service) ensureSourceTenantAdminLocked(ctx context.Context, orgID string, owner *types.User) error {
	if owner == nil || owner.TenantID == 0 {
		return nil
	}
	member, err := s.orgService.GetTenantMember(ctx, orgID, owner.TenantID)
	if err != nil {
		return s.ensureTenantMemberLocked(ctx, orgID, owner.TenantID, owner.ID, types.OrgRoleAdmin)
	}
	if member.Role.HasPermission(types.OrgRoleEditor) {
		return nil
	}
	return s.db.WithContext(ctx).
		Model(&types.OrganizationTenantMember{}).
		Where("organization_id = ? AND tenant_id = ?", orgID, owner.TenantID).
		Updates(map[string]interface{}{
			"role":       types.OrgRoleAdmin,
			"updated_at": time.Now(),
		}).Error
}

func (s *Service) ensureOrgUnlimitedLocked(ctx context.Context, org *types.Organization) error {
	if org == nil || org.MemberLimit == 0 {
		return nil
	}
	if err := s.setOrgMemberLimit(ctx, org.ID, 0); err != nil {
		return err
	}
	org.MemberLimit = 0
	return nil
}

func (s *Service) setOrgMemberLimit(ctx context.Context, orgID string, limit int) error {
	return s.db.WithContext(ctx).
		Model(&types.Organization{}).
		Where("id = ?", orgID).
		Updates(map[string]interface{}{
			"member_limit": limit,
			"updated_at":   time.Now(),
		}).Error
}

func (s *Service) findGuideOrg(ctx context.Context) (*types.Organization, error) {
	var org types.Organization
	err := s.db.WithContext(ctx).
		Where("name = ?", ResourceName).
		Order("created_at ASC").
		First(&org).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &org, nil
}

func (s *Service) findGuideKnowledgeBase(ctx context.Context, tenantID uint64) (*types.KnowledgeBase, error) {
	var kb types.KnowledgeBase
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ? AND is_temporary = ?", tenantID, ResourceName, false).
		Order("created_at ASC").
		First(&kb).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &kb, nil
}

func (s *Service) resolveOwnerUser(ctx context.Context, org *types.Organization, preferred *types.User) (*types.User, error) {
	if org != nil {
		if user, err := s.activeUserByID(ctx, org.OwnerID); err != nil {
			return nil, err
		} else if user != nil {
			return user, nil
		}
		if user, err := s.firstActiveUserInTenant(ctx, org.OwnerTenantID); err != nil {
			return nil, err
		} else if user != nil {
			return user, nil
		}
	}
	if usableUser(preferred) {
		return preferred, nil
	}
	return s.firstActiveUser(ctx)
}

func (s *Service) activeUsers(ctx context.Context) ([]types.User, error) {
	var users []types.User
	err := s.db.WithContext(ctx).
		Where("tenant_id <> 0 AND is_active = ?", true).
		Order("is_system_admin DESC, created_at ASC").
		Find(&users).Error
	return users, err
}

func (s *Service) activeUserByID(ctx context.Context, userID string) (*types.User, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, nil
	}
	var user types.User
	err := s.db.WithContext(ctx).
		Where("id = ? AND tenant_id <> 0 AND is_active = ?", userID, true).
		First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Service) firstActiveUserInTenant(ctx context.Context, tenantID uint64) (*types.User, error) {
	if tenantID == 0 {
		return nil, nil
	}
	var user types.User
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND is_active = ?", tenantID, true).
		Order("is_system_admin DESC, created_at ASC").
		First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Service) firstActiveUser(ctx context.Context) (*types.User, error) {
	var user types.User
	err := s.db.WithContext(ctx).
		Where("tenant_id <> 0 AND is_active = ?", true).
		Order("is_system_admin DESC, created_at ASC").
		First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Service) tenantContext(ctx context.Context, user *types.User) context.Context {
	ctx = context.WithValue(ctx, types.TenantIDContextKey, user.TenantID)
	ctx = context.WithValue(ctx, types.UserIDContextKey, user.ID)
	if tenant := s.tenantInfo(ctx, user.TenantID); tenant != nil {
		ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)
	}
	return ctx
}

func (s *Service) tenantInfo(ctx context.Context, tenantID uint64) *types.Tenant {
	if tenantID == 0 {
		return nil
	}
	var tenant types.Tenant
	if err := s.db.WithContext(ctx).First(&tenant, "id = ?", tenantID).Error; err != nil {
		return nil
	}
	return &tenant
}

func usableUser(user *types.User) bool {
	return user != nil && user.ID != "" && user.TenantID != 0 && user.IsActive
}

func isExistingRelationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, apprepo.ErrOrgMemberAlreadyExists) || errors.Is(err, apprepo.ErrKBShareAlreadyExists) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "idx_org_tenant_members_unique") ||
		strings.Contains(msg, "idx_kb_shares_kb_org")
}

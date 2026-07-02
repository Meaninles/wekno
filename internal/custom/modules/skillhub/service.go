package skillhub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	DefaultPreloadedSkillsDir = "skills/preloaded"
	DefaultRuntimeSkillsDir   = ".local-data/runtime-skills"
	shareTypeOrganization     = "organization"
	shareTypeUser             = "user"

	maxLightweightSkillNameRunes         = 64
	maxLightweightSkillDescriptionRunes  = 1024
	maxLightweightSkillInstructionsRunes = 20000
)

var safePathPartPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

type Service struct {
	db *gorm.DB
}

type organizationShareMigration struct {
	ID             string              `gorm:"type:varchar(36);primaryKey"`
	SkillID        string              `gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_skill_org_share"`
	OrganizationID string              `gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_skill_org_share"`
	SharedByUserID string              `gorm:"type:varchar(36);not null"`
	SourceTenantID uint64              `gorm:"not null;index"`
	Permission     types.OrgMemberRole `gorm:"type:varchar(32);not null;default:'viewer'"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      gorm.DeletedAt `gorm:"index"`
}

func (organizationShareMigration) TableName() string {
	return "custom_skill_org_shares"
}

type userShareMigration struct {
	ID             string              `gorm:"type:varchar(36);primaryKey"`
	SkillID        string              `gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_skill_user_share"`
	TargetUserID   string              `gorm:"type:varchar(36);not null;index;uniqueIndex:idx_custom_skill_user_share"`
	SharedByUserID string              `gorm:"type:varchar(36);not null"`
	SourceTenantID uint64              `gorm:"not null;index"`
	Permission     types.OrgMemberRole `gorm:"type:varchar(32);not null;default:'viewer'"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      gorm.DeletedAt `gorm:"index"`
}

func (userShareMigration) TableName() string {
	return "custom_skill_user_shares"
}

func NewService(db *gorm.DB) *Service {
	service := &Service{db: db}
	registerProfessionalAccessResolver(service.professionalAccessByName)
	return service
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	db := s.db.Session(&gorm.Session{NewDB: true})
	config := *db.Config
	config.DisableForeignKeyConstraintWhenMigrating = true
	db.Config = &config
	if err := db.WithContext(ctx).AutoMigrate(&Skill{}, &ProfessionalSkill{}, &organizationShareMigration{}, &userShareMigration{}); err != nil {
		return err
	}
	if db.Migrator().HasIndex(&ProfessionalSkill{}, "idx_custom_professional_skills_name") {
		if err := db.Migrator().DropIndex(&ProfessionalSkill{}, "idx_custom_professional_skills_name"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ListAccessible(ctx context.Context) ([]SkillListItem, error) {
	return s.listAccessible(ctx, false)
}

func (s *Service) ListForManage(ctx context.Context) ([]SkillListItem, error) {
	return s.listAccessible(ctx, true)
}

func (s *Service) listAccessible(ctx context.Context, includeDisabledOwn bool) ([]SkillListItem, error) {
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	if tenantID == 0 {
		return nil, fmt.Errorf("tenant_id is required")
	}

	items := make([]SkillListItem, 0)
	var own []Skill
	ownQuery := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if !includeDisabledOwn {
		ownQuery = ownQuery.Where("enabled = ?", true)
	}
	if err := ownQuery.Order("updated_at DESC").Find(&own).Error; err != nil {
		return nil, err
	}
	for _, skill := range own {
		items = append(items, SkillListItem{
			Skill:          skill,
			IsMine:         true,
			SourceTenantID: skill.TenantID,
			Permission:     types.OrgRoleAdmin,
		})
	}

	var orgShares []OrganizationShare
	if err := s.db.WithContext(ctx).
		Preload("Skill").
		Preload("Organization").
		Preload("SharedByUser").
		Joins("JOIN organization_tenant_members otm ON otm.organization_id = custom_skill_org_shares.organization_id AND otm.tenant_id = ?", tenantID).
		Order("custom_skill_org_shares.created_at DESC").
		Find(&orgShares).Error; err != nil {
		return nil, err
	}
	for _, share := range orgShares {
		if share.Skill == nil || !share.Skill.Enabled {
			continue
		}
		item := s.itemFromOrgShare(share, tenantID)
		items = append(items, item)
	}

	if userID != "" {
		var userShares []UserShare
		if err := s.db.WithContext(ctx).
			Preload("Skill").
			Preload("SharedByUser").
			Preload("TargetUser").
			Where("target_user_id = ?", userID).
			Order("created_at DESC").
			Find(&userShares).Error; err != nil {
			return nil, err
		}
		for _, share := range userShares {
			if share.Skill == nil || !share.Skill.Enabled {
				continue
			}
			items = append(items, s.itemFromUserShare(share, tenantID))
		}
	}

	return dedupeItems(items), nil
}

func (s *Service) ListOrganization(ctx context.Context, orgID string) ([]SkillListItem, error) {
	tenantID, _ := types.TenantIDFromContext(ctx)
	if tenantID == 0 {
		return nil, fmt.Errorf("tenant_id is required")
	}
	member, err := s.organizationMember(ctx, orgID, tenantID)
	if err != nil {
		return nil, err
	}
	_ = member

	var shares []OrganizationShare
	if err := s.db.WithContext(ctx).
		Preload("Skill").
		Preload("Organization").
		Preload("SharedByUser").
		Where("organization_id = ?", orgID).
		Order("created_at DESC").
		Find(&shares).Error; err != nil {
		return nil, err
	}
	items := make([]SkillListItem, 0, len(shares))
	for _, share := range shares {
		if share.Skill == nil || !share.Skill.Enabled {
			continue
		}
		items = append(items, s.itemFromOrgShare(share, tenantID))
	}
	return items, nil
}

func (s *Service) Create(ctx context.Context, req SkillRequest) (*Skill, error) {
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	if tenantID == 0 || userID == "" {
		return nil, fmt.Errorf("tenant and user are required")
	}
	if !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleContributor) {
		return nil, fmt.Errorf("permission denied")
	}
	skill := &Skill{
		TenantID:     tenantID,
		CreatorID:    userID,
		Name:         strings.TrimSpace(req.Name),
		Description:  strings.TrimSpace(req.Description),
		Instructions: strings.TrimSpace(req.Instructions),
		Enabled:      true,
	}
	if req.Enabled != nil {
		skill.Enabled = *req.Enabled
	}
	if err := s.validateSkill(ctx, skill, ""); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Create(skill).Error; err != nil {
		return nil, err
	}
	return skill, nil
}

func (s *Service) Update(ctx context.Context, id string, req SkillRequest) (*Skill, error) {
	skill, err := s.GetOwnedForManage(ctx, id)
	if err != nil {
		return nil, err
	}
	next := *skill
	next.Name = strings.TrimSpace(req.Name)
	next.Description = strings.TrimSpace(req.Description)
	next.Instructions = strings.TrimSpace(req.Instructions)
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if err := s.validateSkill(ctx, &next, id); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(skill).Updates(map[string]any{
		"name":         next.Name,
		"description":  next.Description,
		"instructions": next.Instructions,
		"enabled":      next.Enabled,
		"updated_at":   time.Now(),
	}).Error; err != nil {
		return nil, err
	}
	return &next, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	skill, err := s.GetOwnedForManage(ctx, id)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Delete(skill).Error
}

func (s *Service) GetOwnedForManage(ctx context.Context, id string) (*Skill, error) {
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	var skill Skill
	if err := s.db.WithContext(ctx).First(&skill, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return nil, err
	}
	if skill.TenantID != tenantID {
		return nil, fmt.Errorf("permission denied")
	}
	if skill.CreatorID != userID && !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin) {
		return nil, fmt.Errorf("permission denied")
	}
	return &skill, nil
}

func (s *Service) ShareToOrganization(ctx context.Context, skillID string, req ShareOrganizationRequest) (*OrganizationShare, error) {
	skill, err := s.GetOwnedForManage(ctx, skillID)
	if err != nil {
		return nil, err
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	member, err := s.organizationMember(ctx, req.OrganizationID, tenantID)
	if err != nil {
		return nil, err
	}
	if !member.Role.HasPermission(types.OrgRoleEditor) {
		return nil, fmt.Errorf("only space editors and admins can receive shared skills")
	}
	permission := normalizePermission(req.Permission)
	share := &OrganizationShare{
		SkillID:        skill.ID,
		OrganizationID: req.OrganizationID,
		SharedByUserID: userID,
		SourceTenantID: tenantID,
		Permission:     permission,
	}
	var existing OrganizationShare
	err = s.db.WithContext(ctx).Where("skill_id = ? AND organization_id = ?", skill.ID, req.OrganizationID).First(&existing).Error
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
	if err := s.db.WithContext(ctx).Create(share).Error; err != nil {
		return nil, err
	}
	return share, nil
}

func (s *Service) ShareToUser(ctx context.Context, skillID string, req ShareUserRequest) (*UserShare, error) {
	skill, err := s.GetOwnedForManage(ctx, skillID)
	if err != nil {
		return nil, err
	}
	userID, _ := types.UserIDFromContext(ctx)
	if strings.TrimSpace(req.UserID) == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	if req.UserID == userID {
		return nil, fmt.Errorf("cannot share a skill to yourself")
	}
	permission := normalizePermission(req.Permission)
	share := &UserShare{
		SkillID:        skill.ID,
		TargetUserID:   req.UserID,
		SharedByUserID: userID,
		SourceTenantID: skill.TenantID,
		Permission:     permission,
	}
	var existing UserShare
	err = s.db.WithContext(ctx).Where("skill_id = ? AND target_user_id = ?", skill.ID, req.UserID).First(&existing).Error
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
	if err := s.db.WithContext(ctx).Create(share).Error; err != nil {
		return nil, err
	}
	return share, nil
}

func (s *Service) ListShares(ctx context.Context, skillID string) (*SkillShareList, error) {
	if _, err := s.GetOwnedForManage(ctx, skillID); err != nil {
		return nil, err
	}
	var orgShares []OrganizationShare
	if err := s.db.WithContext(ctx).
		Preload("Skill").
		Preload("Organization").
		Preload("SharedByUser").
		Where("skill_id = ?", skillID).
		Order("created_at DESC").
		Find(&orgShares).Error; err != nil {
		return nil, err
	}
	var userShares []UserShare
	if err := s.db.WithContext(ctx).
		Preload("Skill").
		Preload("TargetUser").
		Preload("SharedByUser").
		Where("skill_id = ?", skillID).
		Order("created_at DESC").
		Find(&userShares).Error; err != nil {
		return nil, err
	}
	out := &SkillShareList{}
	tenantID, _ := types.TenantIDFromContext(ctx)
	for _, share := range orgShares {
		out.OrganizationShares = append(out.OrganizationShares, s.itemFromOrgShare(share, tenantID))
	}
	for _, share := range userShares {
		out.UserShares = append(out.UserShares, s.itemFromUserShare(share, tenantID))
	}
	return out, nil
}

func (s *Service) RemoveOrganizationShare(ctx context.Context, skillID, shareID string) error {
	if _, err := s.GetOwnedForManage(ctx, skillID); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Where("skill_id = ?", skillID).Delete(&OrganizationShare{ID: shareID}).Error
}

func (s *Service) RemoveUserShare(ctx context.Context, skillID, shareID string) error {
	if _, err := s.GetOwnedForManage(ctx, skillID); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Where("skill_id = ?", skillID).Delete(&UserShare{ID: shareID}).Error
}

func (s *Service) AdditionalMetadata(ctx context.Context) ([]*skills.SkillMetadata, error) {
	items, err := s.ListAccessible(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*skills.SkillMetadata, 0, len(items))
	for _, item := range items {
		out = append(out, &skills.SkillMetadata{
			Name:        item.Name,
			Description: item.Description,
		})
	}
	return out, nil
}

func (s *Service) ConfigureRuntimeSkills(ctx context.Context, req *types.QARequest, agentConfig *types.AgentConfig, customAgent *types.CustomAgent) error {
	if !agentConfig.SkillsEnabled {
		return nil
	}
	names := customAgent.Config.SelectedSkills
	all := customAgent.Config.SkillsSelectionMode == "all"
	dir, materializedNames, err := s.MaterializeAccessible(ctx, names, all)
	if err != nil {
		return err
	}
	if dir == "" {
		return nil
	}
	agentConfig.SkillDirs = appendUnique(agentConfig.SkillDirs, dir)
	if !all && len(agentConfig.AllowedSkills) == 0 {
		agentConfig.AllowedSkills = materializedNames
	}
	_ = req
	return nil
}

func (s *Service) SelectedSkillContext(ctx context.Context, names []string) (string, error) {
	names = normalizeNames(names)
	if len(names) == 0 {
		return "", nil
	}
	accessible, err := s.accessibleByName(ctx)
	if err != nil {
		return "", err
	}
	preloaded := skills.NewLoader([]string{getPreloadedSkillsDir()})
	var sections []string
	for _, name := range names {
		if item, ok := accessible[name]; ok {
			sections = append(sections, renderContextSection(item.Name, item.Description, item.Instructions))
			continue
		}
		if skill, err := preloaded.LoadSkillInstructions(name); err == nil && skill != nil {
			sections = append(sections, renderContextSection(skill.Name, skill.Description, skill.Instructions))
		}
	}
	if len(sections) == 0 {
		return "", nil
	}
	return "[已选择 Skills 上下文]\n" + strings.Join(sections, "\n\n"), nil
}

func (s *Service) AllSkillContext(ctx context.Context) (string, error) {
	accessible, err := s.accessibleByName(ctx)
	if err != nil {
		return "", err
	}
	sections := make([]string, 0, len(accessible))
	names := make([]string, 0, len(accessible))
	for name := range accessible {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		item := accessible[name]
		sections = append(sections, renderContextSection(item.Name, item.Description, item.Instructions))
	}

	preloaded := skills.NewLoader([]string{getPreloadedSkillsDir()})
	metadata, _ := preloaded.DiscoverSkills()
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Name < metadata[j].Name })
	for _, meta := range metadata {
		if skill, err := preloaded.LoadSkillInstructions(meta.Name); err == nil && skill != nil {
			sections = append(sections, renderContextSection(skill.Name, skill.Description, skill.Instructions))
		}
	}
	if len(sections) == 0 {
		return "", nil
	}
	return "[已选择全部轻量 Skills 上下文]\n" + strings.Join(sections, "\n\n"), nil
}

func (s *Service) MaterializeAccessible(ctx context.Context, names []string, all bool) (string, []string, error) {
	accessible, err := s.accessibleByName(ctx)
	if err != nil {
		return "", nil, err
	}
	names = normalizeNames(names)
	selected := make([]Skill, 0)
	if all {
		for _, skill := range accessible {
			selected = append(selected, skill)
		}
		sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	} else {
		for _, name := range names {
			if skill, ok := accessible[name]; ok {
				selected = append(selected, skill)
			}
		}
	}
	if len(selected) == 0 {
		return "", nil, nil
	}
	base := runtimeSkillsDir(ctx)
	if err := os.RemoveAll(base); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(base, 0755); err != nil {
		return "", nil, err
	}
	outNames := make([]string, 0, len(selected))
	for _, skill := range selected {
		dir := filepath.Join(base, safePathPart(skill.Name))
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, skills.SkillFileName), []byte(renderSkillFile(skill.Name, skill.Description, skill.Instructions)), 0644); err != nil {
			return "", nil, err
		}
		outNames = append(outNames, skill.Name)
	}
	return base, outNames, nil
}

func (s *Service) accessibleByName(ctx context.Context) (map[string]Skill, error) {
	items, err := s.ListAccessible(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Skill, len(items))
	for _, item := range items {
		out[item.Name] = item.Skill
	}
	return out, nil
}

func (s *Service) validateSkill(ctx context.Context, skill *Skill, currentID string) error {
	if skill == nil {
		return fmt.Errorf("skill is required")
	}
	skill.Name = strings.TrimSpace(skill.Name)
	skill.Description = strings.TrimSpace(skill.Description)
	skill.Instructions = strings.TrimSpace(skill.Instructions)
	if skill.Name == "" {
		return fmt.Errorf("skill name is required")
	}
	if utf8.RuneCountInString(skill.Name) > maxLightweightSkillNameRunes {
		return fmt.Errorf("skill name must be at most %d characters", maxLightweightSkillNameRunes)
	}
	if strings.ContainsAny(skill.Name, "\r\n\t") {
		return fmt.Errorf("skill name cannot contain line breaks or tabs")
	}
	if utf8.RuneCountInString(skill.Description) > maxLightweightSkillDescriptionRunes {
		return fmt.Errorf("skill description must be at most %d characters", maxLightweightSkillDescriptionRunes)
	}
	if skill.Instructions == "" {
		return fmt.Errorf("skill prompt is required")
	}
	if utf8.RuneCountInString(skill.Instructions) > maxLightweightSkillInstructionsRunes {
		return fmt.Errorf("skill prompt must be at most %d characters", maxLightweightSkillInstructionsRunes)
	}
	var count int64
	query := s.db.WithContext(ctx).Model(&Skill{}).Where("name = ?", skill.Name)
	if currentID != "" {
		query = query.Where("id <> ?", currentID)
	}
	if err := query.Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("skill name already exists")
	}
	if preloadedSkillExists(skill.Name) {
		return fmt.Errorf("skill name conflicts with a preloaded skill")
	}
	return nil
}

func (s *Service) organizationMember(ctx context.Context, orgID string, tenantID uint64) (*types.OrganizationTenantMember, error) {
	var member types.OrganizationTenantMember
	if err := s.db.WithContext(ctx).First(&member, "organization_id = ? AND tenant_id = ?", strings.TrimSpace(orgID), tenantID).Error; err != nil {
		return nil, err
	}
	return &member, nil
}

func (s *Service) itemFromOrgShare(share OrganizationShare, currentTenantID uint64) SkillListItem {
	skill := Skill{}
	if share.Skill != nil {
		skill = *share.Skill
	}
	item := SkillListItem{
		Skill:          skill,
		IsMine:         false,
		ShareID:        share.ID,
		ShareType:      shareTypeOrganization,
		OrganizationID: share.OrganizationID,
		SourceTenantID: share.SourceTenantID,
		Permission:     share.Permission,
		SharedByUserID: share.SharedByUserID,
		SharedAt:       &share.CreatedAt,
	}
	if share.Organization != nil {
		item.OrganizationName = share.Organization.Name
	}
	if share.SharedByUser != nil {
		item.SharedByUsername = share.SharedByUser.Username
	}
	return item
}

func (s *Service) itemFromUserShare(share UserShare, currentTenantID uint64) SkillListItem {
	skill := Skill{}
	if share.Skill != nil {
		skill = *share.Skill
	}
	item := SkillListItem{
		Skill:            skill,
		IsMine:           false,
		ShareID:          share.ID,
		ShareType:        shareTypeUser,
		TargetUserID:     share.TargetUserID,
		SourceTenantID:   share.SourceTenantID,
		Permission:       share.Permission,
		SharedByUserID:   share.SharedByUserID,
		SharedByUsername: "",
		SharedAt:         &share.CreatedAt,
	}
	if share.TargetUser != nil {
		item.TargetUsername = share.TargetUser.Username
	}
	if share.SharedByUser != nil {
		item.SharedByUsername = share.SharedByUser.Username
	}
	return item
}

func dedupeItems(items []SkillListItem) []SkillListItem {
	ownSkillIDs := map[string]bool{}
	for _, item := range items {
		if item.IsMine {
			ownSkillIDs[item.ID] = true
		}
	}

	seen := map[string]bool{}
	out := make([]SkillListItem, 0, len(items))
	for _, item := range items {
		if !item.IsMine && ownSkillIDs[item.ID] {
			continue
		}
		key := fmt.Sprintf("%s:%d:%s", item.ID, item.SourceTenantID, item.ShareType)
		if item.IsMine {
			key = fmt.Sprintf("mine:%s", item.ID)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func normalizePermission(p types.OrgMemberRole) types.OrgMemberRole {
	if p == types.OrgRoleEditor || p == types.OrgRoleAdmin {
		return p
	}
	return types.OrgRoleViewer
}

func normalizeNames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func appendUnique(base []string, values ...string) []string {
	seen := make(map[string]bool, len(base)+len(values))
	out := make([]string, 0, len(base)+len(values))
	for _, value := range append(base, values...) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func renderSkillFile(name, description, instructions string) string {
	frontmatter, _ := yaml.Marshal(map[string]string{
		"name":        strings.TrimSpace(name),
		"description": strings.TrimSpace(description),
	})
	return "---\n" + string(frontmatter) + "---\n\n" + strings.TrimSpace(instructions) + "\n"
}

func renderContextSection(name, description, instructions string) string {
	return fmt.Sprintf("## Skill: %s\n\n%s\n\n%s", strings.TrimSpace(name), strings.TrimSpace(description), strings.TrimSpace(instructions))
}

func preloadedSkillExists(name string) bool {
	loader := skills.NewLoader([]string{getPreloadedSkillsDir()})
	skill, err := loader.LoadSkillInstructions(strings.TrimSpace(name))
	return err == nil && skill != nil
}

func getPreloadedSkillsDir() string {
	if dir := strings.TrimSpace(os.Getenv("WEKNORA_SKILLS_DIR")); dir != "" {
		return dir
	}
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(execPath), DefaultPreloadedSkillsDir)
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		dir := filepath.Join(cwd, DefaultPreloadedSkillsDir)
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	return DefaultPreloadedSkillsDir
}

func runtimeSkillsDir(ctx context.Context) string {
	root := strings.TrimSpace(os.Getenv("WEKNORA_RUNTIME_SKILLS_DIR"))
	if root == "" {
		root = DefaultRuntimeSkillsDir
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	return filepath.Join(root, fmt.Sprintf("%d-%s", tenantID, safePathPart(userID)))
}

func safePathPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "skill"
	}
	value = safePathPartPattern.ReplaceAllString(value, "-")
	value = strings.Trim(value, ".-")
	if value == "" {
		value = "skill"
	}
	if len(value) > 80 {
		value = value[:80]
	}
	return value
}

func (s *Service) DebugLog(ctx context.Context, msg string, args ...any) {
	logger.Debugf(ctx, "[custom skillhub] "+msg, args...)
}

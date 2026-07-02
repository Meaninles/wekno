package configcenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

const managedBy = "custom-config"

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
	if err := s.db.WithContext(ctx).AutoMigrate(&DefaultGrant{}, &UserGrant{}, &ManagedCopy{}); err != nil {
		return err
	}
	return s.cleanupSameTenantManagedCopies(ctx)
}

func (s *Service) ListUsers(ctx context.Context) ([]UserSummary, error) {
	var users []types.User
	if err := s.db.WithContext(ctx).Order("created_at DESC").Find(&users).Error; err != nil {
		return nil, err
	}
	out := make([]UserSummary, 0, len(users))
	for _, user := range users {
		out = append(out, UserSummary{
			ID:       user.ID,
			Username: user.Username,
			TenantID: user.TenantID,
			Active:   user.IsActive,
		})
	}
	return out, nil
}

func (s *Service) ListSourceResourcesForUser(ctx context.Context, sourceUserID string) ([]ResourceSummary, error) {
	sourceUserID = strings.TrimSpace(sourceUserID)
	if sourceUserID == "" {
		return nil, fmt.Errorf("source_user_id is required")
	}
	var user types.User
	if err := s.db.WithContext(ctx).First(&user, "id = ?", sourceUserID).Error; err != nil {
		return nil, err
	}
	if user.TenantID == 0 {
		return nil, fmt.Errorf("source user has no home workspace")
	}
	return s.ListSourceResources(ctx, user.TenantID)
}

func (s *Service) ListSourceResources(ctx context.Context, sourceTenantID uint64) ([]ResourceSummary, error) {
	if sourceTenantID == 0 {
		return nil, fmt.Errorf("source_tenant_id is required")
	}

	excludedIDs, err := s.managedTargetIDs(ctx, sourceTenantID)
	if err != nil {
		return nil, err
	}
	collector := newResourceCollector()

	var models []types.Model
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND (managed_by = '' OR managed_by IS NULL)", sourceTenantID).
		Order("type, display_name, name").
		Find(&models).Error; err != nil {
		return nil, err
	}
	for _, item := range models {
		if isExcludedResource(excludedIDs, ResourceModel, item.ID) {
			continue
		}
		name := strings.TrimSpace(item.DisplayName)
		if name == "" {
			name = item.Name
		}
		collector.add(ResourceSummary{
			ResourceType:   ResourceModel,
			ID:             item.ID,
			SourceTenantID: sourceTenantID,
			Name:           name,
			Description:    item.Description,
			Kind:           string(item.Type),
			Enabled:        true,
		}, modelResourceKey(item))
	}

	var vectorStores []types.VectorStore
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", sourceTenantID).Order("name").Find(&vectorStores).Error; err != nil {
		return nil, err
	}
	for _, item := range vectorStores {
		if isExcludedResource(excludedIDs, ResourceVectorStore, item.ID) {
			continue
		}
		collector.add(ResourceSummary{
			ResourceType:   ResourceVectorStore,
			ID:             item.ID,
			SourceTenantID: sourceTenantID,
			Name:           item.Name,
			Kind:           string(item.EngineType),
			Enabled:        true,
		}, genericResourceKey(ResourceVectorStore, string(item.EngineType), item.Name))
	}

	var providers []types.WebSearchProviderEntity
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", sourceTenantID).Order("name").Find(&providers).Error; err != nil {
		return nil, err
	}
	for _, item := range providers {
		if isExcludedResource(excludedIDs, ResourceWebSearch, item.ID) {
			continue
		}
		collector.add(ResourceSummary{
			ResourceType:   ResourceWebSearch,
			ID:             item.ID,
			SourceTenantID: sourceTenantID,
			Name:           item.Name,
			Description:    item.Description,
			Kind:           string(item.Provider),
			Enabled:        true,
		}, genericResourceKey(ResourceWebSearch, string(item.Provider), item.Name))
	}

	var mcpServices []types.MCPService
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", sourceTenantID).Order("name").Find(&mcpServices).Error; err != nil {
		return nil, err
	}
	for _, item := range mcpServices {
		if isExcludedResource(excludedIDs, ResourceMCP, item.ID) {
			continue
		}
		urlValue := ""
		if item.URL != nil {
			urlValue = *item.URL
		}
		collector.add(ResourceSummary{
			ResourceType:   ResourceMCP,
			ID:             item.ID,
			SourceTenantID: sourceTenantID,
			Name:           item.Name,
			Description:    item.Description,
			Kind:           string(item.TransportType),
			Enabled:        item.Enabled,
		}, genericResourceKey(ResourceMCP, string(item.TransportType), item.Name, urlValue))
	}

	var tenant types.Tenant
	if err := s.db.WithContext(ctx).First(&tenant, "id = ?", sourceTenantID).Error; err != nil {
		return nil, err
	}
	if tenant.ParserEngineConfig != nil && !isExcludedResource(excludedIDs, ResourceParser, TenantConfigResourceID) {
		collector.add(ResourceSummary{
			ResourceType:   ResourceParser,
			ID:             TenantConfigResourceID,
			SourceTenantID: sourceTenantID,
			Name:           "解析引擎配置",
			Kind:           "tenant",
			Enabled:        true,
		}, genericResourceKey(ResourceParser, fmt.Sprintf("%d", sourceTenantID)))
	}
	if tenant.StorageEngineConfig != nil && !isExcludedResource(excludedIDs, ResourceStorage, TenantConfigResourceID) {
		collector.add(ResourceSummary{
			ResourceType:   ResourceStorage,
			ID:             TenantConfigResourceID,
			SourceTenantID: sourceTenantID,
			Name:           "存储引擎配置",
			Kind:           "tenant",
			Enabled:        true,
		}, genericResourceKey(ResourceStorage, fmt.Sprintf("%d", sourceTenantID)))
	}

	return collector.items, nil
}

type resourceCollector struct {
	items []ResourceSummary
	seen  map[string]bool
}

func newResourceCollector() *resourceCollector {
	return &resourceCollector{
		items: []ResourceSummary{},
		seen:  map[string]bool{},
	}
}

func (c *resourceCollector) add(item ResourceSummary, configKey string) {
	configKey = strings.TrimSpace(configKey)
	if configKey == "" {
		configKey = genericResourceKey(item.ResourceType, fmt.Sprintf("%d", item.SourceTenantID), item.ID)
	}
	item.ConfigKey = configKey
	if c.seen[configKey] {
		return
	}
	c.seen[configKey] = true
	c.items = append(c.items, item)
}

func (s *Service) managedTargetIDs(ctx context.Context, tenantID uint64) (map[string]map[string]bool, error) {
	var rows []ManagedCopy
	if err := s.db.WithContext(ctx).Where("target_tenant_id = ?", tenantID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]map[string]bool{}
	for _, row := range rows {
		if row.TargetResourceID == "" {
			continue
		}
		if out[row.ResourceType] == nil {
			out[row.ResourceType] = map[string]bool{}
		}
		out[row.ResourceType][row.TargetResourceID] = true
	}
	return out, nil
}

func isExcludedResource(excluded map[string]map[string]bool, resourceType, id string) bool {
	if excluded == nil || strings.TrimSpace(id) == "" {
		return false
	}
	return excluded[resourceType] != nil && excluded[resourceType][id]
}

func modelResourceKey(item types.Model) string {
	params := item.Parameters
	params.APIKey = ""
	params.AppSecret = ""
	return genericResourceKey(
		ResourceModel,
		string(item.Type),
		string(item.Source),
		item.Name,
		params.Provider,
		params.BaseURL,
		params.InterfaceType,
		params.ParameterSize,
		fmt.Sprintf("%d", params.EmbeddingParameters.Dimension),
		fmt.Sprintf("%d", params.EmbeddingParameters.TruncatePromptTokens),
		fmt.Sprintf("%t", params.EmbeddingParameters.SupportsDimensionOverride),
		fmt.Sprintf("%t", params.SupportsVision),
		hashValue(params.ExtraConfig),
	)
}

func genericResourceKey(parts ...string) string {
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(part)))
	}
	return strings.Join(normalized, "\x1f")
}

func (s *Service) GetDefaults(ctx context.Context) ([]ResourceRef, error) {
	var grants []DefaultGrant
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Order("resource_type, created_at").Find(&grants).Error; err != nil {
		return nil, err
	}
	out := make([]ResourceRef, 0, len(grants))
	for _, grant := range grants {
		out = append(out, refFromDefault(grant))
	}
	return out, nil
}

func (s *Service) SaveDefaults(ctx context.Context, refs []ResourceRef) error {
	normalized, err := normalizeRefs(refs)
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Where("id <> ?", "").Delete(&DefaultGrant{}).Error; err != nil {
			return err
		}
		for _, ref := range normalized {
			grant := DefaultGrant{
				ResourceType:     ref.ResourceType,
				SourceTenantID:   ref.SourceTenantID,
				SourceResourceID: ref.SourceResourceID,
				Enabled:          true,
			}
			if err := tx.Create(&grant).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return s.pruneAllManagedCopies(ctx)
}

func (s *Service) GetUserGrants(ctx context.Context, userID string) ([]ResourceRef, error) {
	var grants []UserGrant
	if err := s.db.WithContext(ctx).Where("user_id = ? AND enabled = ?", userID, true).Order("resource_type, created_at").Find(&grants).Error; err != nil {
		return nil, err
	}
	out := make([]ResourceRef, 0, len(grants))
	for _, grant := range grants {
		out = append(out, refFromUser(grant))
	}
	return out, nil
}

func (s *Service) SaveUserGrants(ctx context.Context, userID string, refs []ResourceRef) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user_id is required")
	}
	normalized, err := normalizeRefs(refs)
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Where("user_id = ?", userID).Delete(&UserGrant{}).Error; err != nil {
			return err
		}
		for _, ref := range normalized {
			grant := UserGrant{
				UserID:           userID,
				ResourceType:     ref.ResourceType,
				SourceTenantID:   ref.SourceTenantID,
				SourceResourceID: ref.SourceResourceID,
				Enabled:          true,
			}
			if err := tx.Create(&grant).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return s.pruneManagedCopiesForUserID(ctx, userID)
}

func (s *Service) ApplyAll(ctx context.Context) (*ApplyResult, error) {
	var users []types.User
	if err := s.db.WithContext(ctx).Where("is_active = ?", true).Find(&users).Error; err != nil {
		return nil, err
	}
	result := &ApplyResult{}
	for _, user := range users {
		applied, err := s.applyUser(ctx, &user)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", user.Username, err))
			continue
		}
		result.UsersApplied++
		result.Resources += applied
	}
	return result, nil
}

func (s *Service) ApplyUserByID(ctx context.Context, userID string) (*ApplyResult, error) {
	var user types.User
	if err := s.db.WithContext(ctx).First(&user, "id = ?", userID).Error; err != nil {
		return nil, err
	}
	count, err := s.applyUser(ctx, &user)
	if err != nil {
		return nil, err
	}
	return &ApplyResult{UsersApplied: 1, Resources: count}, nil
}

func (s *Service) EnsureUserProvisioned(ctx context.Context, user *types.User) error {
	if s == nil || user == nil || user.ID == "" || user.TenantID == 0 {
		return nil
	}
	_, err := s.applyUser(ctx, user)
	return err
}

func (s *Service) applyUser(ctx context.Context, user *types.User) (int, error) {
	refs, err := s.refsForUser(ctx, user.ID)
	if err != nil {
		return 0, err
	}
	if _, err := s.pruneUserManagedCopies(ctx, user, refs); err != nil {
		return 0, err
	}
	count := 0
	var errs []string
	for _, ref := range refs {
		if err := s.applyResource(ctx, user, ref); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", ref.ResourceType, ref.SourceResourceID, err))
			continue
		}
		count++
	}
	if len(errs) > 0 {
		return count, errors.New(strings.Join(errs, "; "))
	}
	return count, nil
}

func (s *Service) refsForUser(ctx context.Context, userID string) ([]ResourceRef, error) {
	defaults, err := s.GetDefaults(ctx)
	if err != nil {
		return nil, err
	}
	userGrants, err := s.GetUserGrants(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := map[string]ResourceRef{}
	for _, ref := range append(defaults, userGrants...) {
		seen[refKey(ref)] = ref
	}
	out := make([]ResourceRef, 0, len(seen))
	for _, ref := range seen {
		out = append(out, ref)
	}
	return out, nil
}

func (s *Service) pruneAllManagedCopies(ctx context.Context) error {
	var users []types.User
	if err := s.db.WithContext(ctx).Where("tenant_id <> 0").Find(&users).Error; err != nil {
		return err
	}
	var errs []string
	for _, user := range users {
		if err := s.pruneManagedCopiesForUser(ctx, &user); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", user.Username, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *Service) pruneManagedCopiesForUserID(ctx context.Context, userID string) error {
	var user types.User
	if err := s.db.WithContext(ctx).First(&user, "id = ?", userID).Error; err != nil {
		return err
	}
	return s.pruneManagedCopiesForUser(ctx, &user)
}

func (s *Service) pruneManagedCopiesForUser(ctx context.Context, user *types.User) error {
	refs, err := s.refsForUser(ctx, user.ID)
	if err != nil {
		return err
	}
	_, err = s.pruneUserManagedCopies(ctx, user, refs)
	return err
}

func (s *Service) pruneUserManagedCopies(ctx context.Context, user *types.User, effectiveRefs []ResourceRef) (int, error) {
	if user == nil || user.ID == "" || user.TenantID == 0 {
		return 0, nil
	}
	effective := map[string]bool{}
	for _, ref := range effectiveRefs {
		effective[refKey(ref)] = true
	}
	var copies []ManagedCopy
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND target_tenant_id = ?", user.ID, user.TenantID).
		Find(&copies).Error; err != nil {
		return 0, err
	}
	pruned := 0
	var errs []string
	for _, copyRow := range copies {
		if effective[refKey(refFromManagedCopy(copyRow))] {
			continue
		}
		if err := s.removeManagedCopy(ctx, user, &copyRow); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", copyRow.ResourceType, copyRow.SourceResourceID, err))
			continue
		}
		pruned++
	}
	if len(errs) > 0 {
		return pruned, errors.New(strings.Join(errs, "; "))
	}
	return pruned, nil
}

func (s *Service) removeManagedCopy(ctx context.Context, user *types.User, copyRow *ManagedCopy) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.deleteManagedCopyTarget(ctx, tx, user, copyRow); err != nil {
			return err
		}
		return tx.Unscoped().Delete(copyRow).Error
	})
}

func (s *Service) deleteManagedCopyTarget(ctx context.Context, tx *gorm.DB, user *types.User, copyRow *ManagedCopy) error {
	targetID := strings.TrimSpace(copyRow.TargetResourceID)
	if targetID == "" {
		return nil
	}
	if targetID == strings.TrimSpace(copyRow.SourceResourceID) &&
		copyRow.ResourceType != ResourceParser &&
		copyRow.ResourceType != ResourceStorage {
		return nil
	}
	if referenced, err := s.targetReferencedByOtherManagedCopy(ctx, tx, copyRow); err != nil {
		return err
	} else if referenced {
		return nil
	}
	switch copyRow.ResourceType {
	case ResourceModel:
		return tx.Where("id = ? AND tenant_id = ? AND managed_by = ?", targetID, user.TenantID, managedBy).
			Delete(&types.Model{}).Error
	case ResourceVectorStore:
		return tx.Where("id = ? AND tenant_id = ?", targetID, user.TenantID).
			Delete(&types.VectorStore{}).Error
	case ResourceWebSearch:
		return tx.Where("id = ? AND tenant_id = ?", targetID, user.TenantID).
			Delete(&types.WebSearchProviderEntity{}).Error
	case ResourceMCP:
		return tx.Where("id = ? AND tenant_id = ? AND is_builtin = ?", targetID, user.TenantID, false).
			Delete(&types.MCPService{}).Error
	case ResourceParser:
		return s.clearTenantConfigIfUnchanged(ctx, tx, user, copyRow, ResourceParser)
	case ResourceStorage:
		return s.clearTenantConfigIfUnchanged(ctx, tx, user, copyRow, ResourceStorage)
	default:
		return nil
	}
}

func (s *Service) targetReferencedByOtherManagedCopy(ctx context.Context, tx *gorm.DB, copyRow *ManagedCopy) (bool, error) {
	targetID := strings.TrimSpace(copyRow.TargetResourceID)
	if targetID == "" {
		return false, nil
	}
	var count int64
	query := tx.WithContext(ctx).Model(&ManagedCopy{}).
		Where("target_tenant_id = ? AND resource_type = ? AND target_resource_id = ?",
			copyRow.TargetTenantID, copyRow.ResourceType, targetID)
	if strings.TrimSpace(copyRow.ID) != "" {
		query = query.Where("id <> ?", copyRow.ID)
	}
	if err := query.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Service) clearTenantConfigIfUnchanged(ctx context.Context, tx *gorm.DB, user *types.User, copyRow *ManagedCopy, resourceType string) error {
	if copyRow.SourceTenantID == user.TenantID {
		return nil
	}
	var target types.Tenant
	if err := tx.WithContext(ctx).First(&target, "id = ?", user.TenantID).Error; err != nil {
		return err
	}
	var source types.Tenant
	if err := tx.WithContext(ctx).First(&source, "id = ?", copyRow.SourceTenantID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	switch resourceType {
	case ResourceParser:
		if target.ParserEngineConfig == nil {
			return nil
		}
		expected, err := applyTemplate(source.ParserEngineConfig, templateVars(user))
		if err != nil {
			return err
		}
		if source.ParserEngineConfig != nil && hashValue(target.ParserEngineConfig) != hashValue(expected) {
			return nil
		}
		return tx.Model(&types.Tenant{}).Where("id = ?", user.TenantID).
			Update("parser_engine_config", gorm.Expr("NULL")).Error
	case ResourceStorage:
		if target.StorageEngineConfig == nil {
			return nil
		}
		expected, err := applyTemplate(source.StorageEngineConfig, templateVars(user))
		if err != nil {
			return err
		}
		if source.StorageEngineConfig != nil && hashValue(target.StorageEngineConfig) != hashValue(expected) {
			return nil
		}
		return tx.Model(&types.Tenant{}).Where("id = ?", user.TenantID).
			Update("storage_engine_config", gorm.Expr("NULL")).Error
	default:
		return nil
	}
}

func (s *Service) applyResource(ctx context.Context, user *types.User, ref ResourceRef) error {
	if user != nil && ref.SourceTenantID == user.TenantID {
		// Tenant-scoped resources already exist in the user's workspace. Default
		// grants must reference them directly instead of cloning "(2)/(3)" copies.
		return s.removeSameTenantManagedCopiesForRef(ctx, user, ref)
	}

	copyRow, err := s.getOrCreateManagedCopy(ctx, user, ref)
	if err != nil {
		return err
	}

	var applyErr error
	var hash string
	switch ref.ResourceType {
	case ResourceModel:
		hash, applyErr = s.applyModel(ctx, user, copyRow, ref)
	case ResourceVectorStore:
		hash, applyErr = s.applyVectorStore(ctx, user, copyRow, ref)
	case ResourceParser:
		hash, applyErr = s.applyParserConfig(ctx, user, copyRow, ref)
	case ResourceStorage:
		hash, applyErr = s.applyStorageConfig(ctx, user, copyRow, ref)
	case ResourceWebSearch:
		hash, applyErr = s.applyWebSearchProvider(ctx, user, copyRow, ref)
	case ResourceMCP:
		hash, applyErr = s.applyMCPService(ctx, user, copyRow, ref)
	default:
		applyErr = fmt.Errorf("unsupported resource type %q", ref.ResourceType)
	}

	now := time.Now()
	if applyErr != nil {
		copyRow.Status = "error"
		copyRow.LastError = applyErr.Error()
		_ = s.db.WithContext(ctx).Save(copyRow).Error
		return applyErr
	}
	copyRow.Status = "active"
	copyRow.SourceHash = hash
	copyRow.LastError = ""
	copyRow.LastAppliedAt = &now
	return s.db.WithContext(ctx).Save(copyRow).Error
}

func (s *Service) getOrCreateManagedCopy(ctx context.Context, user *types.User, ref ResourceRef) (*ManagedCopy, error) {
	var row ManagedCopy
	err := s.db.WithContext(ctx).Where(
		"user_id = ? AND target_tenant_id = ? AND resource_type = ? AND source_tenant_id = ? AND source_resource_id = ?",
		user.ID,
		user.TenantID,
		ref.ResourceType,
		ref.SourceTenantID,
		ref.SourceResourceID,
	).First(&row).Error
	if err == nil {
		if row.TargetResourceID == "" {
			row.TargetResourceID = s.newTargetResourceID(ctx, user.TenantID, ref, row.ID)
		}
		return &row, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return &ManagedCopy{
		UserID:           user.ID,
		TargetTenantID:   user.TenantID,
		ResourceType:     ref.ResourceType,
		SourceTenantID:   ref.SourceTenantID,
		SourceResourceID: ref.SourceResourceID,
		TargetResourceID: s.newTargetResourceID(ctx, user.TenantID, ref, ""),
		Status:           "pending",
	}, nil
}

func (s *Service) applyModel(ctx context.Context, user *types.User, copyRow *ManagedCopy, ref ResourceRef) (string, error) {
	var source types.Model
	if err := s.db.WithContext(ctx).First(&source, "id = ? AND tenant_id = ?", ref.SourceResourceID, ref.SourceTenantID).Error; err != nil {
		return "", err
	}
	target := source
	target.ID = copyRow.TargetResourceID
	target.TenantID = user.TenantID
	target.IsBuiltin = false
	target.ManagedBy = managedBy
	target.DeletedAt = gorm.DeletedAt{}
	target.UpdatedAt = time.Now()
	if target.CreatedAt.IsZero() {
		target.CreatedAt = time.Now()
	}
	if err := s.db.WithContext(ctx).Save(&target).Error; err != nil {
		return "", err
	}
	return hashValue(source), nil
}

func (s *Service) applyVectorStore(ctx context.Context, user *types.User, copyRow *ManagedCopy, ref ResourceRef) (string, error) {
	var source types.VectorStore
	if err := s.db.WithContext(ctx).First(&source, "id = ? AND tenant_id = ?", ref.SourceResourceID, ref.SourceTenantID).Error; err != nil {
		return "", err
	}
	vars := templateVars(user)
	conn, err := applyTemplate(source.ConnectionConfig, vars)
	if err != nil {
		return "", err
	}
	index, err := applyTemplate(source.IndexConfig, vars)
	if err != nil {
		return "", err
	}
	target := source
	target.ID = copyRow.TargetResourceID
	target.TenantID = user.TenantID
	target.Name = s.uniqueVectorStoreName(ctx, user.TenantID, source.Name, copyRow.TargetResourceID)
	target.ConnectionConfig = conn
	target.IndexConfig = index
	target.DeletedAt = gorm.DeletedAt{}
	target.UpdatedAt = time.Now()
	if target.CreatedAt.IsZero() {
		target.CreatedAt = time.Now()
	}
	if err := s.db.WithContext(ctx).Save(&target).Error; err != nil {
		return "", err
	}
	return hashValue(source), nil
}

func (s *Service) applyWebSearchProvider(ctx context.Context, user *types.User, copyRow *ManagedCopy, ref ResourceRef) (string, error) {
	var source types.WebSearchProviderEntity
	if err := s.db.WithContext(ctx).First(&source, "id = ? AND tenant_id = ?", ref.SourceResourceID, ref.SourceTenantID).Error; err != nil {
		return "", err
	}
	params, err := applyTemplate(source.Parameters, templateVars(user))
	if err != nil {
		return "", err
	}
	target := source
	target.ID = copyRow.TargetResourceID
	target.TenantID = user.TenantID
	target.Name = s.uniqueWebSearchName(ctx, user.TenantID, source.Name, copyRow.TargetResourceID)
	target.Parameters = params
	target.DeletedAt = gorm.DeletedAt{}
	target.UpdatedAt = time.Now()
	if target.CreatedAt.IsZero() {
		target.CreatedAt = time.Now()
	}
	if err := s.db.WithContext(ctx).Save(&target).Error; err != nil {
		return "", err
	}
	return hashValue(source), nil
}

func (s *Service) applyMCPService(ctx context.Context, user *types.User, copyRow *ManagedCopy, ref ResourceRef) (string, error) {
	var source types.MCPService
	if err := s.db.WithContext(ctx).First(&source, "id = ? AND tenant_id = ?", ref.SourceResourceID, ref.SourceTenantID).Error; err != nil {
		return "", err
	}
	target, err := applyTemplate(source, templateVars(user))
	if err != nil {
		return "", err
	}
	target.ID = copyRow.TargetResourceID
	target.TenantID = user.TenantID
	target.Name = s.uniqueMCPName(ctx, user.TenantID, source.Name, copyRow.TargetResourceID)
	target.IsBuiltin = false
	target.DeletedAt = gorm.DeletedAt{}
	target.UpdatedAt = time.Now()
	if target.CreatedAt.IsZero() {
		target.CreatedAt = time.Now()
	}
	if err := s.db.WithContext(ctx).Save(&target).Error; err != nil {
		return "", err
	}
	return hashValue(source), nil
}

func (s *Service) applyParserConfig(ctx context.Context, user *types.User, copyRow *ManagedCopy, ref ResourceRef) (string, error) {
	var source types.Tenant
	if err := s.db.WithContext(ctx).First(&source, "id = ?", ref.SourceTenantID).Error; err != nil {
		return "", err
	}
	cfg, err := applyTemplate(source.ParserEngineConfig, templateVars(user))
	if err != nil {
		return "", err
	}
	if err := s.db.WithContext(ctx).Model(&types.Tenant{}).Where("id = ?", user.TenantID).Update("parser_engine_config", cfg).Error; err != nil {
		return "", err
	}
	copyRow.TargetResourceID = TenantConfigResourceID
	return hashValue(source.ParserEngineConfig), nil
}

func (s *Service) applyStorageConfig(ctx context.Context, user *types.User, copyRow *ManagedCopy, ref ResourceRef) (string, error) {
	var source types.Tenant
	if err := s.db.WithContext(ctx).First(&source, "id = ?", ref.SourceTenantID).Error; err != nil {
		return "", err
	}
	cfg, err := applyTemplate(source.StorageEngineConfig, templateVars(user))
	if err != nil {
		return "", err
	}
	if err := s.db.WithContext(ctx).Model(&types.Tenant{}).Where("id = ?", user.TenantID).Update("storage_engine_config", cfg).Error; err != nil {
		return "", err
	}
	copyRow.TargetResourceID = TenantConfigResourceID
	return hashValue(source.StorageEngineConfig), nil
}

func (s *Service) uniqueVectorStoreName(ctx context.Context, tenantID uint64, base, currentID string) string {
	return s.uniqueName(ctx, &types.VectorStore{}, tenantID, base, currentID)
}

func (s *Service) uniqueWebSearchName(ctx context.Context, tenantID uint64, base, currentID string) string {
	return s.uniqueName(ctx, &types.WebSearchProviderEntity{}, tenantID, base, currentID)
}

func (s *Service) uniqueMCPName(ctx context.Context, tenantID uint64, base, currentID string) string {
	return s.uniqueName(ctx, &types.MCPService{}, tenantID, base, currentID)
}

func (s *Service) uniqueName(ctx context.Context, model any, tenantID uint64, base, currentID string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "managed-config"
	}
	for i := 0; i < 20; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s (%d)", base, i+1)
		}
		var count int64
		query := s.db.WithContext(ctx).Model(model).Where("tenant_id = ? AND name = ?", tenantID, name)
		if strings.TrimSpace(currentID) != "" {
			query = query.Where("id <> ?", currentID)
		}
		if err := query.Count(&count).Error; err != nil {
			logger.Warnf(ctx, "[custom config-center] failed to check unique name: %v", err)
			return base
		}
		if count == 0 {
			return name
		}
	}
	return fmt.Sprintf("%s (%s)", base, uuid.NewString()[:8])
}

func refFromDefault(grant DefaultGrant) ResourceRef {
	return ResourceRef{
		ResourceType:     grant.ResourceType,
		SourceTenantID:   grant.SourceTenantID,
		SourceResourceID: grant.SourceResourceID,
	}
}

func refFromUser(grant UserGrant) ResourceRef {
	return ResourceRef{
		ResourceType:     grant.ResourceType,
		SourceTenantID:   grant.SourceTenantID,
		SourceResourceID: grant.SourceResourceID,
	}
}

func refFromManagedCopy(copyRow ManagedCopy) ResourceRef {
	return ResourceRef{
		ResourceType:     copyRow.ResourceType,
		SourceTenantID:   copyRow.SourceTenantID,
		SourceResourceID: copyRow.SourceResourceID,
	}
}

func (s *Service) cleanupSameTenantManagedCopies(ctx context.Context) error {
	var copies []ManagedCopy
	if err := s.db.WithContext(ctx).Where("source_tenant_id = target_tenant_id").Find(&copies).Error; err != nil {
		return err
	}
	var errs []string
	for _, copyRow := range copies {
		user := &types.User{ID: copyRow.UserID, TenantID: copyRow.TargetTenantID}
		if err := s.removeSameTenantManagedCopy(ctx, user, &copyRow); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", copyRow.ResourceType, copyRow.SourceResourceID, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *Service) removeSameTenantManagedCopiesForRef(ctx context.Context, user *types.User, ref ResourceRef) error {
	if user == nil || user.TenantID == 0 {
		return nil
	}
	var copies []ManagedCopy
	if err := s.db.WithContext(ctx).Where(
		"user_id = ? AND target_tenant_id = ? AND resource_type = ? AND source_tenant_id = ? AND source_resource_id = ?",
		user.ID,
		user.TenantID,
		ref.ResourceType,
		user.TenantID,
		ref.SourceResourceID,
	).Find(&copies).Error; err != nil {
		return err
	}
	var errs []string
	for _, copyRow := range copies {
		if err := s.removeSameTenantManagedCopy(ctx, user, &copyRow); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", copyRow.ResourceType, copyRow.SourceResourceID, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *Service) removeSameTenantManagedCopy(ctx context.Context, user *types.User, copyRow *ManagedCopy) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		targetID := strings.TrimSpace(copyRow.TargetResourceID)
		sourceID := strings.TrimSpace(copyRow.SourceResourceID)
		if targetID != "" && targetID != sourceID && targetID != TenantConfigResourceID {
			if err := s.deleteManagedCopyTarget(ctx, tx, user, copyRow); err != nil {
				return err
			}
		}
		return tx.Unscoped().Delete(copyRow).Error
	})
}

func normalizeRefs(refs []ResourceRef) ([]ResourceRef, error) {
	out := make([]ResourceRef, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		ref.ResourceType = strings.TrimSpace(ref.ResourceType)
		ref.SourceResourceID = strings.TrimSpace(ref.SourceResourceID)
		if !isSupportedResourceType(ref.ResourceType) {
			return nil, fmt.Errorf("unsupported resource type %q", ref.ResourceType)
		}
		if ref.SourceTenantID == 0 {
			return nil, fmt.Errorf("source_tenant_id is required")
		}
		if ref.ResourceType == ResourceParser || ref.ResourceType == ResourceStorage {
			ref.SourceResourceID = TenantConfigResourceID
		}
		if ref.SourceResourceID == "" {
			return nil, fmt.Errorf("source_resource_id is required")
		}
		key := refKey(ref)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out, nil
}

func refKey(ref ResourceRef) string {
	return fmt.Sprintf("%s:%d:%s", ref.ResourceType, ref.SourceTenantID, ref.SourceResourceID)
}

func isSupportedResourceType(value string) bool {
	for _, item := range ResourceTypes {
		if item == value {
			return true
		}
	}
	return false
}

func (s *Service) newTargetResourceID(ctx context.Context, targetTenantID uint64, ref ResourceRef, excludeCopyID string) string {
	if ref.ResourceType == ResourceParser || ref.ResourceType == ResourceStorage {
		return TenantConfigResourceID
	}
	if existing := s.reusableManagedTargetID(ctx, targetTenantID, ref, excludeCopyID); existing != "" {
		return existing
	}
	// One physical copy per target tenant and source resource. Multiple users
	// may hold ManagedCopy rows, but they must share the same target row.
	key := fmt.Sprintf("custom-config:%d:%s:%d:%s", targetTenantID, ref.ResourceType, ref.SourceTenantID, ref.SourceResourceID)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

func (s *Service) reusableManagedTargetID(ctx context.Context, targetTenantID uint64, ref ResourceRef, excludeCopyID string) string {
	var existing ManagedCopy
	query := s.db.WithContext(ctx).
		Where("target_tenant_id = ? AND resource_type = ? AND source_tenant_id = ? AND source_resource_id = ? AND target_resource_id <> ?",
			targetTenantID,
			ref.ResourceType,
			ref.SourceTenantID,
			ref.SourceResourceID,
			"",
		)
	if strings.TrimSpace(excludeCopyID) != "" {
		query = query.Where("id <> ?", excludeCopyID)
	}
	if err := query.Order("created_at ASC").First(&existing).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			logger.Warnf(ctx, "[custom config-center] failed to find reusable managed target: %v", err)
		}
		return ""
	}
	targetID := strings.TrimSpace(existing.TargetResourceID)
	if targetID == "" || targetID == TenantConfigResourceID || targetID == strings.TrimSpace(ref.SourceResourceID) {
		return ""
	}
	return targetID
}

func templateVars(user *types.User) map[string]string {
	return map[string]string{
		"${tenant_id}": fmt.Sprintf("%d", user.TenantID),
		"${user_id}":   user.ID,
		"${username}":  user.Username,
	}
}

func applyTemplate[T any](value T, vars map[string]string) (T, error) {
	var zero T
	raw, err := json.Marshal(value)
	if err != nil {
		return zero, err
	}
	text := string(raw)
	for k, v := range vars {
		text = strings.ReplaceAll(text, k, v)
	}
	if err := json.Unmarshal([]byte(text), &zero); err != nil {
		return zero, err
	}
	return zero, nil
}

func hashValue(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

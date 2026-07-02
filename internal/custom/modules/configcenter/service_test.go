package configcenter

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupConfigCenterTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&types.Model{},
		&types.WebSearchProviderEntity{},
		&DefaultGrant{},
		&UserGrant{},
		&ManagedCopy{},
	); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func seedWebSearchProvider(t *testing.T, db *gorm.DB, id string, tenantID uint64, name string) {
	t.Helper()
	provider := &types.WebSearchProviderEntity{
		ID:        id,
		TenantID:  tenantID,
		Name:      name,
		Provider:  types.WebSearchProviderTypeDuckDuckGo,
		IsDefault: true,
	}
	if err := db.Create(provider).Error; err != nil {
		t.Fatalf("seed web search provider: %v", err)
	}
}

func seedModel(t *testing.T, db *gorm.DB, id string, tenantID uint64, name string) {
	t.Helper()
	model := &types.Model{
		ID:        id,
		TenantID:  tenantID,
		Name:      name,
		Type:      types.ModelTypeKnowledgeQA,
		Source:    types.ModelSourceOpenAI,
		Status:    types.ModelStatusActive,
		IsDefault: true,
		Parameters: types.ModelParameters{
			BaseURL:     "https://example.com/v1",
			Provider:    "openai",
			ExtraConfig: map[string]string{},
		},
	}
	if err := db.Create(model).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
}

func liveModelCount(t *testing.T, db *gorm.DB, tenantID uint64) int64 {
	t.Helper()
	var count int64
	if err := db.Model(&types.Model{}).Where("tenant_id = ?", tenantID).Count(&count).Error; err != nil {
		t.Fatalf("count models: %v", err)
	}
	return count
}

func liveProviderCount(t *testing.T, db *gorm.DB, tenantID uint64) int64 {
	t.Helper()
	var count int64
	if err := db.Model(&types.WebSearchProviderEntity{}).Where("tenant_id = ?", tenantID).Count(&count).Error; err != nil {
		t.Fatalf("count providers: %v", err)
	}
	return count
}

func TestApplyWebSearchProviderSkipsSameTenantAndRemovesOldCopies(t *testing.T) {
	ctx := context.Background()
	db := setupConfigCenterTestDB(t)
	service := NewService(db)

	seedWebSearchProvider(t, db, "source-provider", 10000, "duckgo")
	seedWebSearchProvider(t, db, "stale-copy", 10000, "duckgo (2)")
	user := &types.User{ID: "user-1", TenantID: 10000}
	copyRow := &ManagedCopy{
		UserID:           user.ID,
		TargetTenantID:   user.TenantID,
		ResourceType:     ResourceWebSearch,
		SourceTenantID:   user.TenantID,
		SourceResourceID: "source-provider",
		TargetResourceID: "stale-copy",
		Status:           "active",
	}
	if err := db.Create(copyRow).Error; err != nil {
		t.Fatalf("seed managed copy: %v", err)
	}

	ref := ResourceRef{
		ResourceType:     ResourceWebSearch,
		SourceTenantID:   user.TenantID,
		SourceResourceID: "source-provider",
	}
	if err := service.applyResource(ctx, user, ref); err != nil {
		t.Fatalf("apply resource: %v", err)
	}

	if got := liveProviderCount(t, db, user.TenantID); got != 1 {
		t.Fatalf("live provider count = %d, want 1", got)
	}
	var copies int64
	if err := db.Model(&ManagedCopy{}).Count(&copies).Error; err != nil {
		t.Fatalf("count managed copies: %v", err)
	}
	if copies != 0 {
		t.Fatalf("managed copies = %d, want 0", copies)
	}
	var source types.WebSearchProviderEntity
	if err := db.First(&source, "id = ?", "source-provider").Error; err != nil {
		t.Fatalf("source provider should remain live: %v", err)
	}
}

func TestApplyWebSearchProviderReusesManagedTargetAcrossUsersInTenant(t *testing.T) {
	ctx := context.Background()
	db := setupConfigCenterTestDB(t)
	service := NewService(db)

	seedWebSearchProvider(t, db, "source-provider", 10000, "duckgo")
	userA := &types.User{ID: "user-a", TenantID: 10001}
	userB := &types.User{ID: "user-b", TenantID: 10001}
	ref := ResourceRef{
		ResourceType:     ResourceWebSearch,
		SourceTenantID:   10000,
		SourceResourceID: "source-provider",
	}

	if err := service.applyResource(ctx, userA, ref); err != nil {
		t.Fatalf("apply resource for user A: %v", err)
	}
	if err := service.applyResource(ctx, userB, ref); err != nil {
		t.Fatalf("apply resource for user B: %v", err)
	}

	if got := liveProviderCount(t, db, userA.TenantID); got != 1 {
		t.Fatalf("live provider count = %d, want 1", got)
	}
	var copies []ManagedCopy
	if err := db.Order("user_id").Find(&copies).Error; err != nil {
		t.Fatalf("list managed copies: %v", err)
	}
	if len(copies) != 2 {
		t.Fatalf("managed copies = %d, want 2", len(copies))
	}
	if copies[0].TargetResourceID == "" || copies[0].TargetResourceID != copies[1].TargetResourceID {
		t.Fatalf("managed copies should share one target id: %#v", copies)
	}

	if err := service.removeManagedCopy(ctx, userA, &copies[0]); err != nil {
		t.Fatalf("remove first managed copy: %v", err)
	}
	if got := liveProviderCount(t, db, userA.TenantID); got != 1 {
		t.Fatalf("provider should remain while referenced by another copy, got %d", got)
	}
	if err := service.removeManagedCopy(ctx, userB, &copies[1]); err != nil {
		t.Fatalf("remove second managed copy: %v", err)
	}
	if got := liveProviderCount(t, db, userA.TenantID); got != 0 {
		t.Fatalf("provider should be deleted after last reference, got %d", got)
	}
}

func TestApplyModelReusesManagedTargetAcrossUsersInTenant(t *testing.T) {
	ctx := context.Background()
	db := setupConfigCenterTestDB(t)
	service := NewService(db)

	seedModel(t, db, "source-model", 10000, "default-chat")
	userA := &types.User{ID: "user-a", TenantID: 10001}
	userB := &types.User{ID: "user-b", TenantID: 10001}
	ref := ResourceRef{
		ResourceType:     ResourceModel,
		SourceTenantID:   10000,
		SourceResourceID: "source-model",
	}

	if err := service.applyResource(ctx, userA, ref); err != nil {
		t.Fatalf("apply resource for user A: %v", err)
	}
	if err := service.applyResource(ctx, userB, ref); err != nil {
		t.Fatalf("apply resource for user B: %v", err)
	}

	if got := liveModelCount(t, db, userA.TenantID); got != 1 {
		t.Fatalf("live model count = %d, want 1", got)
	}
	var copies []ManagedCopy
	if err := db.Order("user_id").Find(&copies).Error; err != nil {
		t.Fatalf("list managed copies: %v", err)
	}
	if len(copies) != 2 {
		t.Fatalf("managed copies = %d, want 2", len(copies))
	}
	if copies[0].TargetResourceID == "" || copies[0].TargetResourceID != copies[1].TargetResourceID {
		t.Fatalf("managed copies should share one target id: %#v", copies)
	}

	if err := service.removeManagedCopy(ctx, userA, &copies[0]); err != nil {
		t.Fatalf("remove first managed copy: %v", err)
	}
	if got := liveModelCount(t, db, userA.TenantID); got != 1 {
		t.Fatalf("model should remain while referenced by another copy, got %d", got)
	}
	if err := service.removeManagedCopy(ctx, userB, &copies[1]); err != nil {
		t.Fatalf("remove second managed copy: %v", err)
	}
	if got := liveModelCount(t, db, userA.TenantID); got != 0 {
		t.Fatalf("model should be deleted after last reference, got %d", got)
	}
}

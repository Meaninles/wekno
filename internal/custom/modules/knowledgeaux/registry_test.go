package knowledgeaux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var registryTestSequence atomic.Uint64

type fakeFileService struct {
	mu       sync.Mutex
	provider string
	deleted  []string
	failures map[string]int
	started  chan string
	release  <-chan struct{}
}

func (s *fakeFileService) BindingForPath(filePath string) (storagebinding.Binding, error) {
	provider, err := ProviderForPath(filePath, s.provider)
	if err != nil || provider != s.provider {
		return storagebinding.Binding{}, ErrBindingMismatch
	}
	if provider == "local" {
		return storagebinding.Normalize(storagebinding.Binding{
			Provider: storagebinding.ProviderLocal, CanonicalLocalBase: "/fake/" + provider,
			LocalRootIdentity: "fake:" + provider,
			ConfigSource:      storagebinding.ConfigSourceDirect,
			CredentialScope:   storagebinding.CredentialScopeNone,
		})
	}
	parts := strings.SplitN(strings.TrimPrefix(filePath, provider+"://"), "/", 3)
	if len(parts) < 2 || parts[0] == "" {
		return storagebinding.Binding{}, ErrBindingMismatch
	}
	region := "test-region"
	if provider == "cos" {
		region = parts[1]
	}
	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderName(provider), "default",
	)
	if err != nil {
		return storagebinding.Binding{}, err
	}
	binding := storagebinding.Binding{
		Provider: storagebinding.ProviderName(provider), Endpoint: "https://" + provider + ".example.test",
		Region: region, Bucket: parts[0], UseSSL: true,
		ConfigSource:    storagebinding.ConfigSourceDirect,
		CredentialScope: storagebinding.CredentialScopeDirect, CredentialRef: credentialRef,
	}
	if provider == "cos" {
		binding.Endpoint = storagebinding.COSCanonicalEndpoint(binding.Bucket, binding.Region)
	}
	return storagebinding.Normalize(binding)
}

func (s *fakeFileService) CheckConnectivity(context.Context) error { return nil }
func (s *fakeFileService) SaveFile(context.Context, *multipart.FileHeader, uint64, string) (string, error) {
	return "", errors.New("not implemented")
}
func (s *fakeFileService) SaveBytes(context.Context, []byte, uint64, string, bool) (string, error) {
	return "", errors.New("not implemented")
}
func (s *fakeFileService) GetFile(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (s *fakeFileService) GetFileURL(_ context.Context, path string) (string, error) {
	return path, nil
}
func (s *fakeFileService) DeleteFile(_ context.Context, path string) error {
	if s.started != nil {
		s.started <- path
	}
	if s.release != nil {
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, path)
	if s.failures[path] > 0 {
		s.failures[path]--
		return errors.New("injected delete failure")
	}
	return nil
}

type registryKnowledgeCreator struct {
	started chan struct{}
	release <-chan struct{}
	calls   atomic.Int32
}

func (c *registryKnowledgeCreator) CreateKnowledgeTx(
	_ context.Context, tx *gorm.DB, knowledge *types.Knowledge,
) error {
	c.calls.Add(1)
	if err := tx.Create(knowledge).Error; err != nil {
		return err
	}
	if c.started != nil {
		close(c.started)
	}
	if c.release != nil {
		<-c.release
	}
	return nil
}
func (s *fakeFileService) CopyFile(context.Context, string, uint64, string) (string, error) {
	return "", errors.New("not implemented")
}

func openRegistryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), fmt.Sprintf("knowledgeaux-%d.db", registryTestSequence.Add(1))) +
		"?_busy_timeout=5000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.Tenant{}, &types.KnowledgeBase{}, &types.Knowledge{}, &types.Chunk{}, &types.TaskPendingOp{},
	))
	require.NoError(t, db.Create(&types.Tenant{ID: 7, Name: "tenant"}).Error)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "kb"}).Error)
	return db
}

func testRegistry(db *gorm.DB, services map[string]*fakeFileService) *Registry {
	return NewWithResolver(db, func(
		_ context.Context, _ *types.Tenant, provider string,
	) (interfaces.FileService, string, error) {
		service := services[provider]
		if service == nil && services == nil {
			service = &fakeFileService{provider: provider, failures: map[string]int{}}
		}
		if service == nil {
			return nil, provider, fmt.Errorf("provider %s unavailable", provider)
		}
		return service, provider, nil
	})
}

func createOwner(t *testing.T, db *gorm.DB, status, generation string) *types.Knowledge {
	t.Helper()
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		Type: "file", ParseStatus: status, ProcessingGeneration: generation,
	}
	require.NoError(t, db.Create(knowledge).Error)
	return knowledge
}

func tombstoneRegistryKnowledgeBase(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	result := db.Unscoped().Table("knowledge_bases").
		Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", 7, "kb-1").
		Updates(map[string]interface{}{"deleted_at": now, "updated_at": now})
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)
}

func insertRegistryKBDeleteIntent(
	t *testing.T,
	db *gorm.DB,
	tenantID uint64,
	op string,
	dedupKey string,
) {
	t.Helper()
	require.NoError(t, db.Create(&types.TaskPendingOp{
		TenantID: tenantID, TaskType: types.TypeKBDelete,
		Scope: types.TaskScopeKnowledgeBase, ScopeID: "kb-1",
		Op: op, DedupKey: dedupKey,
		Payload:    []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`),
		EnqueuedAt: time.Now().UTC(),
	}).Error)
}

func objectFor(path, generation, kind string) Object {
	provider, err := ProviderForPath(path, "obs")
	if err != nil {
		panic(err)
	}
	binding, err := (&fakeFileService{provider: provider}).BindingForPath(path)
	if err != nil {
		panic(err)
	}
	return Object{
		TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: generation, Path: path, FallbackProvider: "obs", Kind: kind,
		Binding: &binding,
	}
}

func countOwnership(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("task_type = ?", TaskType).Count(&count).Error)
	return count
}

func countAuxOperation(t *testing.T, db *gorm.DB, operation string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("task_type = ? AND op = ?", TaskType, operation).
		Count(&count).Error)
	return count
}

func TestProviderForPathExplicitSchemeWinsOverFallback(t *testing.T) {
	tests := []struct {
		path, fallback, want string
	}{
		{"minio://bucket/key", "obs", "minio"},
		{"local://7/key", "cos", "local"},
		{"https://bucket.cos.ap-guangzhou.myqcloud.com/key", "obs", "cos"},
		{"legacy/without/scheme", "obs", "obs"},
	}
	for _, test := range tests {
		got, err := ProviderForPath(test.path, test.fallback)
		require.NoError(t, err)
		require.Equal(t, test.want, got)
	}
	_, err := ProviderForPath("azure://bucket/key", "obs")
	require.ErrorIs(t, err, ErrProviderRouting)
}

func TestRegisterIsIdempotentAndGenerationFenced(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusProcessing, "generation-1")
	registry := testRegistry(db, nil)
	object := objectFor("local://7/image.png", "generation-1", KindFanoutImage)

	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)
	_, err = registry.Register(context.Background(), object)
	require.NoError(t, err)
	require.EqualValues(t, 1, countOwnership(t, db))

	object.ProcessingGeneration = "generation-2"
	_, err = registry.Register(context.Background(), object)
	require.ErrorIs(t, err, ErrKnowledgeFence)
	require.EqualValues(t, 1, countOwnership(t, db))
}

func TestCleanupKnowledgeRoutesMixedProvidersAndRetainsOnlyFailedPath(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	obs := &fakeFileService{provider: "obs", failures: map[string]int{"obs://bucket/image.png": 1}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local, "obs": obs})
	for _, object := range []Object{
		objectFor("local://7/image.png", "generation-1", KindFanoutImage),
		objectFor("obs://bucket/image.png", "generation-1", KindFanoutImage),
	} {
		_, err := registry.Register(context.Background(), object)
		require.NoError(t, err)
	}

	err := registry.CleanupDerived(context.Background(), 7, "kb-1", "knowledge-1", "obs", nil)
	require.Error(t, err)
	require.EqualValues(t, 1, countOwnership(t, db))
	require.Equal(t, []string{"local://7/image.png"}, local.deleted)
	require.Equal(t, []string{"obs://bucket/image.png"}, obs.deleted)

	require.NoError(t, registry.CleanupDerived(context.Background(), 7, "kb-1", "knowledge-1", "obs", nil))
	require.Zero(t, countOwnership(t, db))
	require.Equal(t, []string{"obs://bucket/image.png", "obs://bucket/image.png"}, obs.deleted)
}

func createLegacyImageChunk(
	t *testing.T,
	db *gorm.DB,
	id string,
	seqID int64,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	path string,
	deleted bool,
) {
	t.Helper()
	chunk := &types.Chunk{
		ID: id, SeqID: seqID, TenantID: tenantID,
		KnowledgeBaseID: knowledgeBaseID, KnowledgeID: knowledgeID,
		ChunkType: types.ChunkTypeText, ImageInfo: fmt.Sprintf(`[{"url":%q}]`, path),
	}
	if deleted {
		chunk.DeletedAt = gorm.DeletedAt{Time: time.Now().Add(-time.Minute), Valid: true}
	}
	require.NoError(t, db.Unscoped().Create(chunk).Error)
}

func TestPrepareDerivedCleanupAdoptsExactOwnerSoftDeletedImageBeforeDelete(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusProcessing, "generation-2")
	path := "local://7/knowledge-1/legacy-image.png"
	createLegacyImageChunk(t, db, "chunk-legacy-owner", 1001, 7, "kb-1", "knowledge-1", path, true)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})

	require.NoError(t, registry.PrepareDerivedCleanup(
		context.Background(), 7, "kb-1", "knowledge-1", "local", []string{path},
	))
	require.EqualValues(t, 1, countOwnership(t, db))
	require.Empty(t, local.deleted, "preflight must never delete provider data")

	require.NoError(t, registry.CleanupDerived(
		context.Background(), 7, "kb-1", "knowledge-1", "local", []string{path},
	))
	require.Zero(t, countOwnership(t, db))
	require.Equal(t, []string{path}, local.deleted)
}

func TestPrepareDerivedCleanupRejectsUnreferencedPathWithoutMutation(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusProcessing, "generation-2")
	path := "local://7/knowledge-1/not-referenced.png"
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})

	err := registry.PrepareDerivedCleanup(
		context.Background(), 7, "kb-1", "knowledge-1", "local", []string{path},
	)
	require.ErrorIs(t, err, ErrBindingMismatch)
	require.Zero(t, countOwnership(t, db))
	require.Empty(t, local.deleted)
}

func TestPrepareDerivedCleanupRejectsCrossKnowledgeReferenceWithoutMutation(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusProcessing, "generation-2")
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-2", TenantID: 7, KnowledgeBaseID: "kb-1",
		Type: "file", ParseStatus: types.ParseStatusCompleted,
		ProcessingGeneration: "generation-other",
	}).Error)
	path := "local://7/knowledge-1/shared-corrupt-image.png"
	createLegacyImageChunk(t, db, "chunk-owner", 1002, 7, "kb-1", "knowledge-1", path, true)
	createLegacyImageChunk(t, db, "chunk-other", 1003, 7, "kb-1", "knowledge-2", path, false)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})

	err := registry.PrepareDerivedCleanup(
		context.Background(), 7, "kb-1", "knowledge-1", "local", []string{path},
	)
	require.ErrorIs(t, err, ErrBindingMismatch)
	require.Zero(t, countOwnership(t, db))
	require.Empty(t, local.deleted)
}

func TestPrepareDerivedCleanupRejectsPathOutsideExactKnowledgeNamespace(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusProcessing, "generation-2")
	path := "local://7/exports/legacy-image.png"
	createLegacyImageChunk(t, db, "chunk-export", 1004, 7, "kb-1", "knowledge-1", path, true)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})

	err := registry.PrepareDerivedCleanup(
		context.Background(), 7, "kb-1", "knowledge-1", "local", []string{path},
	)
	require.ErrorIs(t, err, ErrBindingMismatch)
	require.Zero(t, countOwnership(t, db))
	require.Empty(t, local.deleted)
}

func TestRecoveryCleansStaleFAQEntriesAfterPendingTaskCancellation(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/faq.json", "generation-1", KindFAQEntries)
	object.FallbackProvider = "local"
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("task_type = ?", TaskType).
		Update("enqueued_at", time.Now().Add(-time.Hour)).Error)

	recovery := NewRecoveryWithConfig(registry, RecoveryConfig{
		ScanInterval: time.Hour, ScanTimeout: time.Hour, PendingOwnerGrace: time.Nanosecond,
		FAQEntriesMaxAge: time.Nanosecond, FAQExportMaxAge: time.Nanosecond,
	})
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Zero(t, countOwnership(t, db))
	require.Equal(t, []string{"local://7/faq.json"}, local.deleted)
}

func TestRecoveryDefersTombstonedKnowledgeBaseToDurableDeleteIntent(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
	object.FallbackProvider = "local"
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		tombstoneRegistryKnowledgeBase(t, tx)
		insertRegistryKBDeleteIntent(t, tx, 7, "delete", "kb-1")
		return nil
	}))

	require.NoError(t, NewRecovery(registry).RecoverNow(context.Background()))
	require.EqualValues(t, 1, countOwnership(t, db))
	require.Empty(t, local.deleted)
}

func TestRecoveryRevalidatesDeleteIntentAfterItsSnapshot(t *testing.T) {
	db := openRegistryTestDB(t)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
	object.FallbackProvider = "local"
	_, err := registry.Reserve(context.Background(), object, true)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("task_type = ?", TaskType).
		Update("enqueued_at", time.Now().Add(-2*time.Hour)).Error)

	var rows []*types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", TaskType).Find(&rows).Error)
	require.Len(t, rows, 1)
	recovery := NewRecoveryWithConfig(registry, RecoveryConfig{
		ScanInterval: time.Hour, ScanTimeout: time.Hour, PendingOwnerGrace: time.Nanosecond,
		FAQEntriesMaxAge: time.Hour, FAQExportMaxAge: time.Hour,
	})
	prepared, err := recovery.prepareRecoveryRows(context.Background(), rows, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, prepared, 1)
	require.True(t, prepared[0].recover, "the pre-delete snapshot must consider the stale reservation recoverable")

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		tombstoneRegistryKnowledgeBase(t, tx)
		insertRegistryKBDeleteIntent(t, tx, 7, "delete", "kb-1")
		return nil
	}))
	require.NoError(t, recovery.recoverRow(context.Background(), rows[0], time.Now().UTC()))
	require.EqualValues(t, 1, countOwnership(t, db))
	require.Empty(t, local.deleted)
}

func TestRecoveryStillCleansTombstonedKnowledgeBaseWithoutExactDeleteIntent(t *testing.T) {
	tests := []struct {
		name     string
		tenantID uint64
		op       string
		dedupKey string
	}{
		{name: "missing intent"},
		{name: "other tenant", tenantID: 8, op: "delete", dedupKey: "kb-1"},
		{name: "wrong operation", tenantID: 7, op: "other", dedupKey: "kb-1"},
		{name: "wrong dedup key", tenantID: 7, op: "delete", dedupKey: "other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := openRegistryTestDB(t)
			createOwner(t, db, types.ParseStatusCompleted, "generation-1")
			local := &fakeFileService{provider: "local", failures: map[string]int{}}
			registry := testRegistry(db, map[string]*fakeFileService{"local": local})
			object := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
			object.FallbackProvider = "local"
			_, err := registry.Register(context.Background(), object)
			require.NoError(t, err)
			tombstoneRegistryKnowledgeBase(t, db)
			if test.op != "" {
				insertRegistryKBDeleteIntent(t, db, test.tenantID, test.op, test.dedupKey)
			}

			require.NoError(t, NewRecovery(registry).RecoverNow(context.Background()))
			require.Zero(t, countOwnership(t, db))
			require.Equal(t, []string{object.Path}, local.deleted)
		})
	}
}

func TestRecoveryRetainsReferencedFAQExportAndCleansSupersededExport(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/failed.csv", "generation-1", KindFAQFailedExport)
	object.FallbackProvider = "local"
	object.Reference = "https://download/failed.csv"
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)
	require.NoError(t, owner.SetLastFAQImportResult(&types.FAQImportResult{
		FailedEntriesURL: object.Reference,
	}))
	require.NoError(t, db.Model(owner).Update("last_faq_import_result", owner.LastFAQImportResult).Error)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("task_type = ?", TaskType).
		Update("enqueued_at", time.Now().Add(-time.Hour)).Error)

	recovery := NewRecoveryWithConfig(registry, RecoveryConfig{
		ScanInterval: time.Hour, ScanTimeout: time.Hour, PendingOwnerGrace: time.Nanosecond,
		FAQEntriesMaxAge: time.Nanosecond, FAQExportMaxAge: time.Nanosecond,
	})
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.EqualValues(t, 1, countOwnership(t, db))
	require.Empty(t, local.deleted)

	require.NoError(t, db.Model(owner).Update("last_faq_import_result", types.JSON(nil)).Error)
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Zero(t, countOwnership(t, db))
	require.Equal(t, []string{"local://7/failed.csv"}, local.deleted)
}

func TestPendingSourceReservationSurvivesGraceThenRecoversWithoutKnowledgeRow(t *testing.T) {
	db := openRegistryTestDB(t)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
	object.FallbackProvider = "local"
	object, err := registry.Reserve(context.Background(), object, true)
	require.NoError(t, err)

	recovery := NewRecoveryWithConfig(registry, RecoveryConfig{
		ScanInterval: time.Hour, ScanTimeout: time.Hour, PendingOwnerGrace: time.Hour,
		FAQEntriesMaxAge: time.Hour, FAQExportMaxAge: time.Hour,
	})
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.EqualValues(t, 1, countOwnership(t, db))
	require.Empty(t, local.deleted)

	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("task_type = ?", TaskType).
		Update("enqueued_at", time.Now().Add(-2*time.Hour)).Error)
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Zero(t, countOwnership(t, db))
	require.Equal(t, []string{"local://7/knowledge-1/source.pdf"}, local.deleted)
}

func TestRecoveryRetainsFailedDerivedObjectsForPartialWork(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusFailed, "generation-1")
	path := "local://7/orphan.png"
	local := &fakeFileService{provider: "local", failures: map[string]int{path: 1}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor(path, "generation-1", KindFanoutImage)
	object.FallbackProvider = "local"
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)
	recovery := NewRecovery(registry)

	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.EqualValues(t, 1, countOwnership(t, db))
	require.Empty(t, local.deleted)

	// A new processing generation makes the old derived object unreferenced;
	// provider failure retains proof for the next recovery cycle.
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("processing_generation", "generation-2").Error)
	require.Error(t, recovery.RecoverNow(context.Background()))
	require.EqualValues(t, 1, countOwnership(t, db))
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Zero(t, countOwnership(t, db))
}

func TestPrepareForDeleteAdoptsHistoricalImageAndKeepsRetryIdempotent(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-2")
	legacyPath := "local://7/knowledge-1/generation-1-image.png"
	currentPath := "local://7/knowledge-1/generation-2-image.png"
	sourcePath := "local://7/knowledge-1/source.pdf"
	createLegacyImageChunk(
		t, db, "chunk-generation-1", 1101, 7, "kb-1", "knowledge-1", legacyPath, true,
	)
	require.NoError(t, db.Unscoped().Model(&types.Chunk{}).
		Where("id = ?", "chunk-generation-1").
		Update("processing_generation", "generation-1").Error)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	for _, object := range []Object{
		objectFor(currentPath, "generation-2", KindFanoutImage),
		objectFor(sourcePath, "generation-2", KindSourceFile),
	} {
		object.FallbackProvider = "local"
		_, err := registry.Register(context.Background(), object)
		require.NoError(t, err)
	}
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Updates(map[string]interface{}{
			"parse_status": types.ParseStatusDeleting,
			"file_path":    sourcePath,
		}).Error)
	tombstoneRegistryKnowledgeBase(t, db)
	insertRegistryKBDeleteIntent(t, db, 7, "delete", "kb-1")

	require.NoError(t, registry.PrepareForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		[]string{legacyPath, currentPath},
		[]string{sourcePath},
	))
	require.EqualValues(t, 3, countOwnership(t, db))
	require.Empty(t, local.deleted, "delete preparation must not mutate provider data")

	require.NoError(t, registry.CleanupForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		[]string{legacyPath, currentPath, sourcePath},
	))
	require.EqualValues(t, 3, countAuxOperation(t, db, operationDeleteComplete))
	require.ElementsMatch(t, []string{legacyPath, currentPath, sourcePath}, local.deleted)

	// A later subsystem failure re-enters both the preparation and destructive
	// phases. Existing delete-complete rows must remain authoritative: no new
	// owned row and no second provider delete may be issued.
	require.NoError(t, registry.PrepareForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		[]string{legacyPath, currentPath},
		[]string{sourcePath},
	))
	require.Zero(t, countAuxOperation(t, db, operationOwned))
	require.NoError(t, registry.CleanupForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		[]string{legacyPath, currentPath, sourcePath},
	))
	require.ElementsMatch(t, []string{legacyPath, currentPath, sourcePath}, local.deleted)
}

func TestPrepareForDeleteBatchAdoptsLargeHistoricalImageSet(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-2")
	legacyPaths := make([]string, 0, 140)
	for index := 0; index < 140; index++ {
		path := fmt.Sprintf("local://7/knowledge-1/historical-%03d.png", index)
		legacyPaths = append(legacyPaths, path)
		createLegacyImageChunk(
			t,
			db,
			fmt.Sprintf("chunk-historical-%03d", index),
			int64(2000+index),
			7,
			"kb-1",
			"knowledge-1",
			path,
			true,
		)
	}
	sourcePath := "local://7/knowledge-1/source.pdf"
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	source := objectFor(sourcePath, "generation-2", KindSourceFile)
	source.FallbackProvider = "local"
	_, err := registry.Register(context.Background(), source)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Updates(map[string]interface{}{
			"parse_status": types.ParseStatusDeleting,
			"file_path":    sourcePath,
		}).Error)

	require.NoError(t, registry.PrepareForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		legacyPaths,
		[]string{sourcePath},
	))
	require.EqualValues(t, 141, countOwnership(t, db))
	require.Empty(t, local.deleted)
}

func TestPrepareForDeleteBatchRejectsOneCrossOwnerPathWithoutPartialAdoption(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-2")
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-2", TenantID: 7, KnowledgeBaseID: "kb-1",
		Type: "file", ParseStatus: types.ParseStatusCompleted,
		ProcessingGeneration: "generation-other",
	}).Error)
	goodPath := "local://7/knowledge-1/good-historical.png"
	conflictPath := "local://7/knowledge-1/cross-owner-historical.png"
	createLegacyImageChunk(t, db, "chunk-good-owner", 2201, 7, "kb-1", "knowledge-1", goodPath, true)
	createLegacyImageChunk(t, db, "chunk-conflict-owner", 2202, 7, "kb-1", "knowledge-1", conflictPath, true)
	createLegacyImageChunk(t, db, "chunk-conflict-other", 2203, 7, "kb-1", "knowledge-2", conflictPath, false)
	sourcePath := "local://7/knowledge-1/source.pdf"
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	source := objectFor(sourcePath, "generation-2", KindSourceFile)
	source.FallbackProvider = "local"
	_, err := registry.Register(context.Background(), source)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Updates(map[string]interface{}{
			"parse_status": types.ParseStatusDeleting,
			"file_path":    sourcePath,
		}).Error)

	err = registry.PrepareForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		[]string{goodPath, conflictPath},
		[]string{sourcePath},
	)
	require.ErrorIs(t, err, ErrBindingMismatch)
	require.EqualValues(t, 1, countOwnership(t, db), "the source row is the only pre-existing ownership")
	require.Empty(t, local.deleted)
}

func TestPrepareForDeleteRejectsUnregisteredPersistentPathBeforeProviderMutation(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	currentPath := "local://7/knowledge-1/image.png"
	unregisteredSource := "local://7/knowledge-1/source.pdf"
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	current := objectFor(currentPath, "generation-1", KindFanoutImage)
	current.FallbackProvider = "local"
	_, err := registry.Register(context.Background(), current)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Updates(map[string]interface{}{
			"parse_status": types.ParseStatusDeleting,
			"file_path":    unregisteredSource,
		}).Error)

	err = registry.PrepareForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		[]string{currentPath},
		[]string{unregisteredSource},
	)
	require.ErrorIs(t, err, ErrBindingMissing)
	require.Empty(t, local.deleted)
	require.EqualValues(t, 1, countOwnership(t, db))
}

func TestDerivedCleanupAndRecoveryNeverDeletePersistentSources(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusFailed, "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	objects := []Object{
		objectFor("local://7/source.pdf", "generation-1", KindSourceFile),
		objectFor("local://7/clone-source.pdf", "generation-1", KindCloneSourceFile),
		objectFor("local://7/image.png", "generation-1", KindFanoutImage),
	}
	for _, object := range objects {
		object.FallbackProvider = "local"
		_, err := registry.Register(context.Background(), object)
		require.NoError(t, err)
	}

	recovery := NewRecovery(registry)
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Empty(t, local.deleted, "failed status retains partial derived work and both sources")

	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("processing_generation", "generation-2").Error)
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Equal(t, []string{"local://7/image.png"}, local.deleted)
	require.EqualValues(t, 2, countOwnership(t, db))

	require.NoError(t, registry.CleanupDerived(
		context.Background(), 7, "kb-1", "knowledge-1", "local", nil,
	))
	require.EqualValues(t, 2, countOwnership(t, db))
	require.NoError(t, registry.CleanupForDelete(
		context.Background(), 7, "kb-1", "knowledge-1", "local", nil,
	))
	require.EqualValues(t, 2, countOwnership(t, db))
	require.EqualValues(t, 2, countAuxOperation(t, db, operationDeleteComplete))
	require.ElementsMatch(t, []string{
		"local://7/image.png", "local://7/source.pdf", "local://7/clone-source.pdf",
	}, local.deleted)

	// A later subsystem/finalizer failure re-delivers document cleanup. Exact
	// delete-complete proofs make the retry a no-op instead of misclassifying
	// the now-absent FilePath as an unregistered legacy object.
	require.NoError(t, registry.CleanupForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		[]string{"local://7/source.pdf", "local://7/clone-source.pdf"},
	))
	require.ElementsMatch(t, []string{
		"local://7/image.png", "local://7/source.pdf", "local://7/clone-source.pdf",
	}, local.deleted, "retry must not issue a second provider delete")
	require.EqualValues(t, 2, countAuxOperation(t, db, operationDeleteComplete))

	require.NoError(t, registry.PurgeKnowledgeBaseDeleteProofs(
		context.Background(), 7, "kb-1",
	))
	require.Zero(t, countOwnership(t, db))
}

func TestDeleteCompletionProofDoesNotAuthorizeAnotherLegacyPath(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusDeleting, "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	source := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
	source.FallbackProvider = "local"

	// Registration is generation-fenced once deleting starts, so create the
	// ordinary ownership first and then enter the deletion state.
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("parse_status", types.ParseStatusCompleted).Error)
	_, err := registry.Register(context.Background(), source)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("parse_status", types.ParseStatusDeleting).Error)

	require.NoError(t, registry.CleanupForDelete(
		context.Background(), 7, "kb-1", "knowledge-1", "local", []string{source.Path},
	))
	require.Equal(t, []string{source.Path}, local.deleted)

	err = registry.CleanupForDelete(
		context.Background(),
		7,
		"kb-1",
		"knowledge-1",
		"local",
		[]string{source.Path, "local://7/knowledge-1/unowned.txt"},
	)
	require.ErrorIs(t, err, ErrBindingMissing)
	require.Equal(t, []string{source.Path}, local.deleted)
	require.EqualValues(t, 1, countAuxOperation(t, db, operationDeleteComplete))
}

func TestCleanupSkipsClientReferenceForRegisteredFAQExport(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/export.csv", "generation-1", KindFAQFailedExport)
	object.FallbackProvider = "local"
	object.Reference = "https://download.example/export.csv"
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)

	require.NoError(t, registry.CleanupForDelete(
		context.Background(), 7, "kb-1", "knowledge-1", "local", []string{object.Reference},
	))
	require.Equal(t, []string{object.Path}, local.deleted)
}

func TestKnowledgeBaseScopedLedgerIsolation(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusProcessing, "generation-1")
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-2", TenantID: 7, Name: "kb-2"}).Error)
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-2", TenantID: 7, KnowledgeBaseID: "kb-2", Type: "file",
		ParseStatus: types.ParseStatusProcessing, ProcessingGeneration: "generation-2",
	}).Error)
	registry := testRegistry(db, nil)
	_, err := registry.Register(context.Background(), objectFor(
		"local://7/kb-1.png", "generation-1", KindFanoutImage,
	))
	require.NoError(t, err)
	second := objectFor("local://7/kb-2.png", "generation-2", KindFanoutImage)
	second.KnowledgeBaseID = "kb-2"
	second.KnowledgeID = "knowledge-2"
	_, err = registry.Register(context.Background(), second)
	require.NoError(t, err)

	count1, err := registry.CountKnowledgeBase(context.Background(), 7, "kb-1")
	require.NoError(t, err)
	count2, err := registry.CountKnowledgeBase(context.Background(), 7, "kb-2")
	require.NoError(t, err)
	require.EqualValues(t, 1, count1)
	require.EqualValues(t, 1, count2)

	var rows []types.TaskPendingOp
	require.NoError(t, db.Order("id").Find(&rows).Error)
	require.Equal(t, types.TaskScopeKnowledgeBase, rows[0].Scope)
	require.Equal(t, "kb-1", rows[0].ScopeID)
	require.Contains(t, rows[0].DedupKey, "knowledge-1:")
	require.Equal(t, "kb-2", rows[1].ScopeID)
}

func TestFileServiceForPathRequiresExactRegisteredKBTuple(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusProcessing, "generation-1")
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/faq.json", "generation-1", KindFAQEntries)
	object.FallbackProvider = "local"
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)

	service, err := registry.FileServiceForPath(
		context.Background(), 7, "kb-1", "knowledge-1", object.Path, "obs",
	)
	require.NoError(t, err)
	require.Same(t, local, service)
	_, err = registry.FileServiceForPath(
		context.Background(), 7, "wrong-kb", "knowledge-1", object.Path, "local",
	)
	require.ErrorIs(t, err, ErrReservationLost)
	_, err = registry.FileServiceForPath(
		context.Background(), 7, "kb-1", "knowledge-1", "", "local",
	)
	require.ErrorIs(t, err, ErrInvalidObject)
}

func TestRecoveryFirstPreventsLateReservationAdoption(t *testing.T) {
	db := openRegistryTestDB(t)
	releaseDelete := make(chan struct{})
	deleteStarted := make(chan string, 1)
	local := &fakeFileService{
		provider: "local", failures: map[string]int{}, started: deleteStarted, release: releaseDelete,
	}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
	object.FallbackProvider = "local"
	object, err := registry.Reserve(context.Background(), object, true)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("task_type = ?", TaskType).
		Update("enqueued_at", time.Now().Add(-2*time.Hour)).Error)

	recovery := NewRecoveryWithConfig(registry, RecoveryConfig{
		ScanInterval: time.Hour, ScanTimeout: time.Hour, PendingOwnerGrace: time.Nanosecond,
		FAQEntriesMaxAge: time.Hour, FAQExportMaxAge: time.Hour,
	})
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- recovery.RecoverNow(context.Background()) }()
	require.Equal(t, object.Path, <-deleteStarted)

	creator := &registryKnowledgeCreator{}
	knowledge := &types.Knowledge{
		ID: object.KnowledgeID, TenantID: object.TenantID, KnowledgeBaseID: object.KnowledgeBaseID,
		Type: "file", FilePath: object.Path, ParseStatus: types.ParseStatusPending,
		ProcessingGeneration: object.ProcessingGeneration,
	}
	adoptDone := make(chan error, 1)
	go func() {
		adoptDone <- registry.AdoptReservedKnowledge(context.Background(), object, knowledge, creator)
	}()
	select {
	case err := <-adoptDone:
		t.Fatalf("adoption escaped recovery lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseDelete)
	require.NoError(t, <-recoveryDone)
	require.ErrorIs(t, <-adoptDone, ErrReservationLost)
	require.Zero(t, creator.calls.Load())
	var count int64
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", object.KnowledgeID).Count(&count).Error)
	require.Zero(t, count)
}

func TestAdoptionFirstPreventsRecoveryFromDeletingCommittedSource(t *testing.T) {
	db := openRegistryTestDB(t)
	local := &fakeFileService{provider: "local", failures: map[string]int{}, started: make(chan string, 1)}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
	object.FallbackProvider = "local"
	object, err := registry.Reserve(context.Background(), object, true)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("task_type = ?", TaskType).
		Update("enqueued_at", time.Now().Add(-2*time.Hour)).Error)

	creatorStarted := make(chan struct{})
	releaseCreator := make(chan struct{})
	creator := &registryKnowledgeCreator{started: creatorStarted, release: releaseCreator}
	knowledge := &types.Knowledge{
		ID: object.KnowledgeID, TenantID: object.TenantID, KnowledgeBaseID: object.KnowledgeBaseID,
		Type: "file", FilePath: object.Path, ParseStatus: types.ParseStatusPending,
		ProcessingGeneration: object.ProcessingGeneration,
	}
	adoptDone := make(chan error, 1)
	go func() {
		adoptDone <- registry.AdoptReservedKnowledge(context.Background(), object, knowledge, creator)
	}()
	<-creatorStarted

	recovery := NewRecoveryWithConfig(registry, RecoveryConfig{
		ScanInterval: time.Hour, ScanTimeout: time.Hour, PendingOwnerGrace: time.Nanosecond,
		FAQEntriesMaxAge: time.Hour, FAQExportMaxAge: time.Hour,
	})
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- recovery.RecoverNow(context.Background()) }()
	select {
	case path := <-local.started:
		t.Fatalf("recovery deleted while adoption held the KB lock: %s", path)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCreator)
	require.NoError(t, <-adoptDone)
	require.NoError(t, <-recoveryDone)
	require.EqualValues(t, 1, countOwnership(t, db))
	require.Empty(t, local.deleted)
}

func TestDeleteWaitsForCommitAndDeleteFirstSuppressesCommitCallback(t *testing.T) {
	t.Run("commit first", func(t *testing.T) {
		db := openRegistryTestDB(t)
		createOwner(t, db, types.ParseStatusProcessing, "generation-1")
		deleteStarted := make(chan string, 1)
		local := &fakeFileService{provider: "local", failures: map[string]int{}, started: deleteStarted}
		registry := testRegistry(db, map[string]*fakeFileService{"local": local})
		object := objectFor("local://7/image.png", "generation-1", KindFanoutImage)
		object.FallbackProvider = "local"
		object, err := registry.Reserve(context.Background(), object, false)
		require.NoError(t, err)
		commitStarted := make(chan struct{})
		releaseCommit := make(chan struct{})
		commitDone := make(chan error, 1)
		go func() {
			commitDone <- registry.CommitReserved(context.Background(), object, false, func() error {
				close(commitStarted)
				<-releaseCommit
				return nil
			})
		}()
		<-commitStarted
		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- registry.DeletePaths(
				context.Background(), 7, "kb-1", "knowledge-1", "local", []string{object.Path},
			)
		}()
		select {
		case path := <-deleteStarted:
			t.Fatalf("delete escaped commit lock: %s", path)
		case <-time.After(50 * time.Millisecond):
		}
		close(releaseCommit)
		require.NoError(t, <-commitDone)
		require.NoError(t, <-deleteDone)
		require.Equal(t, object.Path, <-deleteStarted)
	})

	t.Run("delete first", func(t *testing.T) {
		db := openRegistryTestDB(t)
		createOwner(t, db, types.ParseStatusProcessing, "generation-1")
		deleteStarted := make(chan string, 1)
		releaseDelete := make(chan struct{})
		local := &fakeFileService{
			provider: "local", failures: map[string]int{}, started: deleteStarted, release: releaseDelete,
		}
		registry := testRegistry(db, map[string]*fakeFileService{"local": local})
		object := objectFor("local://7/image.png", "generation-1", KindFanoutImage)
		object.FallbackProvider = "local"
		object, err := registry.Reserve(context.Background(), object, false)
		require.NoError(t, err)
		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- registry.DeletePaths(
				context.Background(), 7, "kb-1", "knowledge-1", "local", []string{object.Path},
			)
		}()
		require.Equal(t, object.Path, <-deleteStarted)
		var commitCalls atomic.Int32
		commitDone := make(chan error, 1)
		go func() {
			commitDone <- registry.CommitReserved(context.Background(), object, false, func() error {
				commitCalls.Add(1)
				return nil
			})
		}()
		select {
		case err := <-commitDone:
			t.Fatalf("commit escaped delete lock: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(releaseDelete)
		require.NoError(t, <-deleteDone)
		require.ErrorIs(t, <-commitDone, ErrReservationLost)
		require.Zero(t, commitCalls.Load())
	})
}

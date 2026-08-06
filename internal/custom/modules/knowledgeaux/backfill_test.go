package knowledgeaux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

func TestBackfillAdoptsExactLegacySourceIncludingDeletingTombstone(t *testing.T) {
	for _, tombstoned := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "deleting-tombstone"}[tombstoned], func(t *testing.T) {
			db := openRegistryTestDB(t)
			status := types.ParseStatusCompleted
			if tombstoned {
				status = types.ParseStatusDeleting
			}
			owner := createOwner(t, db, status, "generation-1")
			path := "local://7/knowledge-1/source.pdf"
			require.NoError(t, db.Model(owner).Update("file_path", path).Error)
			owner.FilePath = path
			if tombstoned {
				require.NoError(t, db.Delete(owner).Error)
				require.NoError(t, db.Delete(&types.KnowledgeBase{}, "id = ?", "kb-1").Error)
			}
			local := &fakeFileService{provider: "local", failures: map[string]int{}}
			registry := testRegistry(db, map[string]*fakeFileService{"local": local})
			report, err := registry.BackfillLegacyBindings(context.Background())
			require.NoError(t, err)
			require.Equal(t, 1, report.Scanned)
			require.Equal(t, 1, report.Adopted)
			require.Zero(t, report.Quarantined)
			require.EqualValues(t, 1, countOwnership(t, db))
			var row types.TaskPendingOp
			require.NoError(t, db.Where("task_type = ?", TaskType).Take(&row).Error)
			object, err := decodeObject(row.Payload)
			require.NoError(t, err)
			require.NotNil(t, object.Binding)
			require.False(t, object.Quarantined)
		})
	}
}

func TestBackfillLegacyDerivedBindingIsIdempotentAndRepairsOldIdentityQuarantine(t *testing.T) {
	db := openRegistryTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	createOwner(t, db, types.ParseStatusCompleted, "")
	path := "local://7/exports/legacy-generationless.png"
	imageInfo, err := json.Marshal([]*types.ImageInfo{{URL: path}})
	require.NoError(t, err)
	require.NoError(t, db.Create(&types.Chunk{
		ID: "legacy-image-chunk", TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ImageInfo: string(imageInfo),
	}).Error)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})

	first, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, first.Adopted)
	require.Zero(t, first.Quarantined)

	second, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, second.AlreadyRegistered)
	require.Zero(t, second.Adopted)
	require.Zero(t, second.Quarantined)

	var row types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", TaskType).Take(&row).Error)
	object, err := decodeObject(row.Payload)
	require.NoError(t, err)
	object.Quarantined = true
	object.QuarantineReason = quarantineReasonLegacyIdentity
	payload, err := json.Marshal(object)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("id = ?", row.ID).
		Update("payload", payload).Error)

	repaired, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, repaired.Adopted)
	require.Zero(t, repaired.Quarantined)
	require.NoError(t, db.Where("id = ?", row.ID).Take(&row).Error)
	object, err = decodeObject(row.Payload)
	require.NoError(t, err)
	require.False(t, object.Quarantined)
	require.Empty(t, object.QuarantineReason)

	service, err := registry.FileServiceForPath(
		context.Background(), 7, "kb-1", "knowledge-1", path, "local",
	)
	require.NoError(t, err)
	require.Same(t, local, service)
}

func TestBackfillRepairsPersistentSourceOwnershipAfterKnowledgeBaseMove(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-source")
	path := "local://7/knowledge-1/source.pdf"
	require.NoError(t, db.Model(owner).Update("file_path", path).Error)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})

	first, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, first.Adopted)
	require.Zero(t, first.Quarantined)

	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-2", TenantID: 7, Name: "target"}).Error)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", owner.ID).Updates(map[string]interface{}{
		"knowledge_base_id":     "kb-2",
		"processing_generation": "generation-target",
	}).Error)

	// Reproduce the pre-fix startup outcome: the old KB-scoped proof was
	// mistaken for a second logical owner of the same raw source path.
	var row types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", TaskType).Take(&row).Error)
	persisted, err := decodeObject(row.Payload)
	require.NoError(t, err)
	persisted.Quarantined = true
	persisted.QuarantineReason = quarantineReasonSharedPhysical
	payload, err := json.Marshal(persisted)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Where("id = ?", row.ID).
		Update("payload", payload).Error)

	repaired, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, repaired.AlreadyRegistered)
	require.Zero(t, repaired.Quarantined)
	require.EqualValues(t, 1, countOwnership(t, db))

	require.NoError(t, db.Where("id = ?", row.ID).Take(&row).Error)
	require.Equal(t, "kb-2", row.ScopeID)
	persisted, err = decodeObject(row.Payload)
	require.NoError(t, err)
	require.Equal(t, "kb-2", persisted.KnowledgeBaseID)
	require.Equal(t, "generation-target", persisted.ProcessingGeneration)
	require.False(t, persisted.Quarantined)
	require.Empty(t, persisted.QuarantineReason)

	recovery := NewRecoveryWithConfig(registry, RecoveryConfig{
		ScanInterval: time.Minute, ScanTimeout: time.Minute, BackfillTimeout: time.Minute,
		PendingOwnerGrace: time.Millisecond, FAQEntriesMaxAge: time.Hour, FAQExportMaxAge: time.Hour,
	})
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Empty(t, local.deleted, "the moved document's only source file must remain owned")
	require.EqualValues(t, 1, countOwnership(t, db))
}

func TestBackfillSkipsFAQDisplayURLAndRedactsQuarantinePaths(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	require.NoError(t, owner.SetLastFAQImportResult(&types.FAQImportResult{
		FailedEntriesURL: "https://download.example/export.csv?X-Amz-Signature=top-secret",
	}))
	require.NoError(t, db.Model(owner).Update("last_faq_import_result", owner.LastFAQImportResult).Error)
	registry := testRegistry(db, nil)
	report, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Zero(t, report.Scanned)
	require.Equal(t, 1, report.SkippedDisplayURL)
	require.Zero(t, countOwnership(t, db))

	report.quarantine(owner.ID, "plain-internal-token-top-secret", "test-reason")
	require.Len(t, report.QuarantineSamples, 1)
	require.NotContains(t, report.QuarantineSamples[0], "top-secret")
	require.Contains(t, report.QuarantineSamples[0], "path_sha256=")
}

func TestBackfillRejectsSourceOutsideExactOwnerNamespace(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	require.NoError(t, db.Model(owner).Update("file_path", "local://7/someone-else/source.pdf").Error)
	registry := testRegistry(db, nil)
	report, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Scanned)
	require.Equal(t, 1, report.Quarantined)
	require.Equal(t, 1, report.QuarantineReasons["owner-namespace-mismatch"])
	require.Zero(t, countOwnership(t, db))
}

func TestBackfillRejectsNestedLegacyNamespace(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	require.NoError(t, db.Model(owner).Update(
		"file_path", "local://7/knowledge-1/foreign/source.pdf",
	).Error)
	report, err := testRegistry(db, nil).BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Quarantined)
	require.Zero(t, countOwnership(t, db))
}

func TestBackfillSharedImageQuarantinesExistingLedgerAndBlocksReadDelete(t *testing.T) {
	db := openRegistryTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	first := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-2", TenantID: 7, KnowledgeBaseID: "kb-1", Type: "file",
		ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "generation-2",
	}).Error)
	path := "local://7/exports/shared.png"
	imageInfo, err := json.Marshal([]*types.ImageInfo{{URL: path}})
	require.NoError(t, err)
	for _, id := range []string{first.ID, "knowledge-2"} {
		require.NoError(t, db.Create(&types.Chunk{
			ID: "chunk-" + id, TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: id,
			ImageInfo: string(imageInfo),
		}).Error)
	}
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	registry := testRegistry(db, map[string]*fakeFileService{"local": local})
	object := objectFor(path, "generation-1", KindFanoutImage)
	_, err = registry.Register(context.Background(), object)
	require.NoError(t, err)

	report, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, report.Quarantined)
	var row types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", TaskType).Take(&row).Error)
	persisted, err := decodeObject(row.Payload)
	require.NoError(t, err)
	require.True(t, persisted.Quarantined)
	require.Equal(t, "shared_physical_object", persisted.QuarantineReason)
	_, err = registry.FileServiceForPath(context.Background(), 7, "kb-1", first.ID, path, "local")
	require.ErrorIs(t, err, ErrBindingQuarantined)
	err = registry.DeletePaths(context.Background(), 7, "kb-1", first.ID, "local", []string{path})
	require.ErrorIs(t, err, ErrBindingQuarantined)
	require.Empty(t, local.deleted)
}

func TestPhysicalDigestMergesAliasesAndIgnoresCredentialProfile(t *testing.T) {
	directRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeDirect, storagebinding.ProviderCOS, "default",
	)
	require.NoError(t, err)
	direct, err := storagebinding.Normalize(storagebinding.Binding{
		Provider: storagebinding.ProviderCOS, Region: "ap-test", Bucket: "bucket-123",
		PathPrefix: "tenant", UseSSL: true, ConfigSource: storagebinding.ConfigSourceDirect,
		CredentialScope: storagebinding.CredentialScopeDirect, CredentialRef: directRef,
	})
	require.NoError(t, err)
	tenantRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeTenant, storagebinding.ProviderCOS, "default",
	)
	require.NoError(t, err)
	tenant := direct
	tenant.Fingerprint = ""
	tenant.ConfigSource = storagebinding.ConfigSourceTenant
	tenant.CredentialScope = storagebinding.CredentialScopeTenant
	tenant.CredentialRef = tenantRef
	tenant, err = storagebinding.Normalize(tenant)
	require.NoError(t, err)
	require.NotEqual(t, direct.Fingerprint, tenant.Fingerprint)

	schemeKey, err := bindingObjectKey(direct, "cos://bucket-123/ap-test/tenant/7/knowledge-1/a.png")
	require.NoError(t, err)
	httpsKey, err := bindingObjectKey(tenant, "https://bucket-123.cos.ap-test.myqcloud.com/tenant/7/knowledge-1/a.png")
	require.NoError(t, err)
	require.Equal(t, schemeKey, httpsKey)
	require.Equal(t, physicalObjectDigest(direct, schemeKey), physicalObjectDigest(tenant, httpsKey))
}

func TestBackfillTimeoutIsRetryable(t *testing.T) {
	db := openRegistryTestDB(t)
	registry := testRegistry(db, nil)
	recovery := NewRecovery(registry)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := recovery.RunBackfill(cancelled)
	require.Error(t, err)
	require.False(t, recovery.bindingBackfillComplete())
	_, err = recovery.RunBackfill(context.Background())
	require.NoError(t, err)
	require.True(t, recovery.bindingBackfillComplete())
}

func TestRegisterWithoutBindingOrConcreteServiceFailsWithoutResolving(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	resolverCalls := 0
	registry := NewWithResolver(db, func(context.Context, *types.Tenant, string) (interfaces.FileService, string, error) {
		resolverCalls++
		return nil, "", errors.New("must not be called")
	})
	object := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
	object.Binding = nil
	_, err := registry.Register(context.Background(), object)
	require.ErrorIs(t, err, ErrBindingMissing)
	require.Zero(t, resolverCalls)
}

func TestLedgerReferenceNeverPersistsPresignedQuery(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	registry := testRegistry(db, nil)
	object := objectFor("local://7/knowledge-1/export.csv", "generation-1", KindFAQFailedExport)
	object.Reference = "https://download.example/export.csv?token=top-secret"
	_, err := registry.Register(context.Background(), object)
	require.NoError(t, err)
	var row types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", TaskType).Take(&row).Error)
	require.False(t, strings.Contains(string(row.Payload), "top-secret"))
	require.Contains(t, string(row.Payload), referenceFingerprintPrefix)
}

func TestBackfillSanitizesRawLegacyReferenceWithoutLosingUnknownFields(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	payload := []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1","knowledge_id":"knowledge-1",` +
		`"processing_generation":"generation-1","path":"local://7/knowledge-1/export.csv",` +
		`"reference":"https://download.example/export.csv?token=top-secret","kind":"faq_failed_export",` +
		`"future_field":{"preserve":true}}`)
	require.NoError(t, db.Create(&types.TaskPendingOp{
		TenantID: 7, TaskType: TaskType, Scope: types.TaskScopeKnowledgeBase, ScopeID: "kb-1",
		Op: operationOwned, DedupKey: objectKey("knowledge-1", "local://7/knowledge-1/export.csv"),
		Payload: payload, EnqueuedAt: time.Now().UTC(),
	}).Error)

	_, err := testRegistry(db, nil).BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	var row types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", TaskType).Take(&row).Error)
	require.NotContains(t, string(row.Payload), "top-secret")
	require.Contains(t, string(row.Payload), referenceFingerprintPrefix)
	require.Contains(t, string(row.Payload), `"future_field":{"preserve":true}`)
}

func TestBackfillSanitizesRawReferenceWhenKnowledgeBaseIsMissing(t *testing.T) {
	db := openRegistryTestDB(t)
	payload, err := json.Marshal(Object{
		TenantID: 7, KnowledgeBaseID: "missing-kb", KnowledgeID: "knowledge-gone",
		ProcessingGeneration: "generation-1", Path: "local://7/knowledge-gone/export.csv",
		Reference: "https://download.example/export.csv?signature=top-secret", Kind: KindFAQFailedExport,
	})
	require.NoError(t, err)
	require.NoError(t, db.Create(&types.TaskPendingOp{
		TenantID: 7, TaskType: TaskType, Scope: types.TaskScopeKnowledgeBase, ScopeID: "missing-kb",
		Op: operationOwned, DedupKey: objectKey("knowledge-gone", "local://7/knowledge-gone/export.csv"),
		Payload: payload, EnqueuedAt: time.Now().UTC(),
	}).Error)
	_, err = testRegistry(db, nil).BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	var row types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", TaskType).Take(&row).Error)
	require.NotContains(t, string(row.Payload), "top-secret")
}

func TestBackfillRawConflictQuarantinesProvableOwner(t *testing.T) {
	db := openRegistryTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	first := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	path := "local://7/knowledge-1/source.pdf"
	require.NoError(t, db.Model(first).Update("file_path", path).Error)
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-2", TenantID: 7, KnowledgeBaseID: "kb-1", Type: "file",
		ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "generation-2",
	}).Error)
	imageInfo, err := json.Marshal([]*types.ImageInfo{{URL: path}})
	require.NoError(t, err)
	require.NoError(t, db.Create(&types.Chunk{
		ID: "chunk-2", TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-2",
		ImageInfo: string(imageInfo),
	}).Error)
	report, err := testRegistry(db, nil).BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Zero(t, report.Adopted)
	require.Zero(t, countOwnership(t, db))
	require.GreaterOrEqual(t, report.QuarantineReasons["shared-raw-path"], 1)
}

func TestBackfillIgnoresSoftDeletedChunksForActiveOwner(t *testing.T) {
	db := openRegistryTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	imageInfo, err := json.Marshal([]*types.ImageInfo{{URL: "local://7/exports/stale.png"}})
	require.NoError(t, err)
	chunk := &types.Chunk{
		ID: "stale-chunk", TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ImageInfo: string(imageInfo),
	}
	require.NoError(t, db.Create(chunk).Error)
	require.NoError(t, db.Delete(chunk).Error)
	report, err := testRegistry(db, nil).BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Zero(t, report.Scanned)
	require.Zero(t, countOwnership(t, db))
}

func TestWalkLegacyCandidatesDoesNotSkipInterleavedTenantUUIDs(t *testing.T) {
	db := openRegistryTestDB(t)
	rows := make([]*types.Knowledge, 0, backfillBatchSize+5)
	for index := 0; index < backfillBatchSize; index++ {
		id := fmt.Sprintf("z-%03d", index)
		rows = append(rows, &types.Knowledge{
			ID: id, TenantID: 7, KnowledgeBaseID: "kb-1", Type: "file",
			ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "g-1",
			FilePath: "local://7/" + id + "/source.pdf",
		})
	}
	for index := 0; index < 5; index++ {
		id := fmt.Sprintf("a-%03d", index)
		rows = append(rows, &types.Knowledge{
			ID: id, TenantID: 8, KnowledgeBaseID: "kb-2", Type: "file",
			ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "g-2",
			FilePath: "local://8/" + id + "/source.pdf",
		})
	}
	require.NoError(t, db.CreateInBatches(rows, 100).Error)
	visited := 0
	_, err := testRegistry(db, nil).walkLegacyCandidates(context.Background(), func(
		_ *types.Knowledge, _ legacyCandidate,
	) error {
		visited++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, backfillBatchSize+5, visited)
}

func TestLegacyIdentityQuarantineReasonDecodesFailClosed(t *testing.T) {
	object := objectFor("local://7/knowledge-1/source.pdf", "generation-1", KindSourceFile)
	object.Quarantined = true
	object.QuarantineReason = quarantineReasonLegacyIdentity
	payload, err := json.Marshal(object)
	require.NoError(t, err)
	decoded, err := decodeObject(payload)
	require.NoError(t, err)
	require.True(t, decoded.Quarantined)
}

type generationRaceService struct {
	*fakeFileService
	mu      sync.Mutex
	calls   int
	trigger int
	hook    func()
}

func (s *generationRaceService) BindingForPath(path string) (storagebinding.Binding, error) {
	s.mu.Lock()
	s.calls++
	trigger := s.calls == s.trigger
	s.mu.Unlock()
	if trigger {
		s.hook()
	}
	return s.fakeFileService.BindingForPath(path)
}

func TestBackfillDoesNotSignDerivedPathWhenGenerationChangesDuringPass(t *testing.T) {
	db := openRegistryTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	path := "local://7/exports/race.png"
	imageInfo, err := json.Marshal([]*types.ImageInfo{{URL: path}})
	require.NoError(t, err)
	require.NoError(t, db.Create(&types.Chunk{
		ID: "race-chunk", TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ImageInfo: string(imageInfo),
	}).Error)
	service := &generationRaceService{
		fakeFileService: &fakeFileService{provider: "local", failures: map[string]int{}},
		trigger:         3,
		hook: func() {
			require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
				Update("processing_generation", "generation-2").Error)
		},
	}
	registry := NewWithResolver(db, func(
		context.Context, *types.Tenant, string,
	) (interfaces.FileService, string, error) {
		return service, "local", nil
	})
	_, err = registry.BackfillLegacyBindings(context.Background())
	require.ErrorIs(t, err, errBackfillTargetChanged)
	require.Zero(t, countOwnership(t, db))
}

func TestBackfillDoesNotAdoptImageSoftDeletedBetweenPasses(t *testing.T) {
	db := openRegistryTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	path := "local://7/exports/race-delete.png"
	imageInfo, err := json.Marshal([]*types.ImageInfo{{URL: path}})
	require.NoError(t, err)
	chunk := &types.Chunk{
		ID: "race-delete-chunk", TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ImageInfo: string(imageInfo),
	}
	require.NoError(t, db.Create(chunk).Error)
	service := &generationRaceService{
		fakeFileService: &fakeFileService{provider: "local", failures: map[string]int{}},
		trigger:         3,
		hook: func() {
			require.NoError(t, db.Delete(chunk).Error)
		},
	}
	registry := NewWithResolver(db, func(
		context.Context, *types.Tenant, string,
	) (interfaces.FileService, string, error) {
		return service, "local", nil
	})
	report, err := registry.BackfillLegacyBindings(context.Background())
	require.NoError(t, err)
	require.Zero(t, report.Adopted)
	require.Zero(t, countOwnership(t, db))
}

type targetFileService struct {
	*fakeFileService
	root string
}

func (s *targetFileService) BindingForPath(path string) (storagebinding.Binding, error) {
	if !strings.HasPrefix(path, "local://") {
		return storagebinding.Binding{}, ErrBindingMismatch
	}
	return storagebinding.Normalize(storagebinding.Binding{
		Provider: storagebinding.ProviderLocal, CanonicalLocalBase: s.root,
		LocalRootIdentity: "root:" + s.root, ConfigSource: storagebinding.ConfigSourceDirect,
		CredentialScope: storagebinding.CredentialScopeNone,
	})
}

func TestBackfillCommitRejectsStorageTargetDrift(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	path := "local://7/knowledge-1/source.pdf"
	require.NoError(t, db.Model(owner).Update("file_path", path).Error)
	first := &targetFileService{
		fakeFileService: &fakeFileService{provider: "local", failures: map[string]int{}}, root: "/target/one",
	}
	second := &targetFileService{
		fakeFileService: &fakeFileService{provider: "local", failures: map[string]int{}}, root: "/target/two",
	}
	resolverCalls := 0
	registry := NewWithResolver(db, func(
		context.Context, *types.Tenant, string,
	) (interfaces.FileService, string, error) {
		resolverCalls++
		if resolverCalls == 1 {
			return first, "local", nil
		}
		return second, "local", nil
	})
	_, err := registry.BackfillLegacyBindings(context.Background())
	require.ErrorIs(t, err, errBackfillTargetChanged)
	require.Zero(t, countOwnership(t, db))
}

func TestRecoveryCursorReachesOrphanAfterRetainedFirstPage(t *testing.T) {
	db := openRegistryTestDB(t)
	createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	rows := make([]*types.TaskPendingOp, 0, 1001)
	for index := 0; index < 1000; index++ {
		path := fmt.Sprintf("local://7/knowledge-1/source-%04d.pdf", index)
		object := objectFor(path, "generation-1", KindSourceFile)
		payload, err := json.Marshal(object)
		require.NoError(t, err)
		rows = append(rows, &types.TaskPendingOp{
			TenantID: 7, TaskType: TaskType, Scope: types.TaskScopeKnowledgeBase, ScopeID: "kb-1",
			Op: operationOwned, DedupKey: objectKey("knowledge-1", path), Payload: payload,
			EnqueuedAt: time.Now().Add(-time.Hour),
		})
	}
	orphanPath := "local://7/missing-owner/source.pdf"
	orphan := objectFor(orphanPath, "generation-orphan", KindSourceFile)
	orphan.KnowledgeID = "missing-owner"
	orphanPayload, err := json.Marshal(orphan)
	require.NoError(t, err)
	rows = append(rows, &types.TaskPendingOp{
		TenantID: 7, TaskType: TaskType, Scope: types.TaskScopeKnowledgeBase, ScopeID: "kb-1",
		Op: operationOwned, DedupKey: objectKey("missing-owner", orphanPath), Payload: orphanPayload,
		EnqueuedAt: time.Now().Add(-time.Hour),
	})
	require.NoError(t, db.CreateInBatches(rows, 100).Error)
	local := &fakeFileService{provider: "local", failures: map[string]int{}}
	recovery := NewRecoveryWithConfig(testRegistry(db, map[string]*fakeFileService{"local": local}), RecoveryConfig{
		ScanInterval: time.Minute, ScanTimeout: time.Minute, BackfillTimeout: time.Minute,
		PendingOwnerGrace: time.Millisecond, FAQEntriesMaxAge: time.Hour, FAQExportMaxAge: time.Hour,
		BatchSize: 1000,
	})
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Empty(t, local.deleted)
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Equal(t, []string{orphanPath}, local.deleted)
}

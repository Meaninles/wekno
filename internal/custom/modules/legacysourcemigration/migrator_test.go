package legacysourcemigration

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type memoryOBS struct {
	mu       sync.Mutex
	binding  storagebinding.Binding
	objects  map[string][]byte
	metadata map[string]string
}

func newMemoryOBS(t *testing.T) *memoryOBS {
	t.Helper()
	credentialRef, err := storagebinding.CredentialProfileReference(
		storagebinding.CredentialScopeGlobal,
		storagebinding.ProviderOBS,
		"default",
	)
	require.NoError(t, err)
	binding, err := storagebinding.Normalize(storagebinding.Binding{
		Provider:        storagebinding.ProviderOBS,
		Endpoint:        "https://obs.example.test",
		Region:          "test-region",
		Bucket:          "bucket-a",
		PathPrefix:      "private/deployment-a",
		UseSSL:          true,
		ConfigSource:    storagebinding.ConfigSourceGlobal,
		CredentialScope: storagebinding.CredentialScopeGlobal,
		CredentialRef:   credentialRef,
	})
	require.NoError(t, err)
	return &memoryOBS{
		binding:  binding,
		objects:  make(map[string][]byte),
		metadata: make(map[string]string),
	}
}

func (s *memoryOBS) BindingForPath(filePath string) (storagebinding.Binding, error) {
	if _, err := plannedfile.ParseBucketPath(
		filePath,
		"obs",
		s.binding.Bucket,
		s.binding.PathPrefix,
	); err != nil {
		return storagebinding.Binding{}, err
	}
	return s.binding, nil
}

func (s *memoryOBS) CheckConnectivity(context.Context) error { return nil }
func (s *memoryOBS) SaveFile(context.Context, *multipart.FileHeader, uint64, string) (string, error) {
	return "", errors.New("not implemented")
}
func (s *memoryOBS) SaveBytes(context.Context, []byte, uint64, string, bool) (string, error) {
	return "", errors.New("not implemented")
}
func (s *memoryOBS) GetFile(_ context.Context, filePath string) (io.ReadCloser, error) {
	if _, err := plannedfile.ParseBucketPath(filePath, "obs", s.binding.Bucket, ""); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[filePath]
	if !ok {
		return nil, errors.New("NoSuchKey")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}
func (s *memoryOBS) GetFileURL(_ context.Context, filePath string) (string, error) {
	return filePath, nil
}
func (s *memoryOBS) DeleteFile(_ context.Context, filePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, filePath)
	delete(s.metadata, filePath)
	return nil
}
func (s *memoryOBS) CopyFile(context.Context, string, uint64, string) (string, error) {
	return "", errors.New("not implemented")
}
func (s *memoryOBS) ReservePrivateObjectPath(segments ...string) (string, error) {
	key, err := plannedfile.BuildKey(s.binding.PathPrefix, segments...)
	if err != nil {
		return "", err
	}
	return plannedfile.FormatBucketPath("obs", s.binding.Bucket, key)
}
func (s *memoryOBS) CommitPrivateObjectAtPath(
	ctx context.Context,
	data []byte,
	filePath string,
	_ string,
	digest string,
) error {
	return s.commit(ctx, bytes.NewReader(data), int64(len(data)), filePath, digest)
}
func (s *memoryOBS) CommitPrivateObjectStreamAtPath(
	ctx context.Context,
	reader io.ReadSeeker,
	size int64,
	filePath string,
	_ string,
	digest string,
) error {
	return s.commit(ctx, reader, size, filePath, digest)
}
func (s *memoryOBS) commit(
	_ context.Context,
	reader io.Reader,
	size int64,
	filePath string,
	digest string,
) error {
	if _, err := s.BindingForPath(filePath); err != nil {
		return err
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errors.New("size mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[filePath] = append([]byte(nil), data...)
	s.metadata[filePath] = strings.ToLower(strings.TrimSpace(digest))
	return nil
}
func (s *memoryOBS) VerifyPrivateObject(
	_ context.Context,
	filePath string,
	size int64,
	digest string,
) error {
	if _, err := s.BindingForPath(filePath); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[filePath]
	if !ok || int64(len(data)) != size ||
		s.metadata[filePath] != strings.ToLower(strings.TrimSpace(digest)) {
		return errors.New("private object verification failed")
	}
	return nil
}

func openMigrationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(
		filepath.Join(t.TempDir(), "legacy-source.db")+"?_busy_timeout=5000&_journal_mode=WAL",
	), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.Tenant{},
		&types.KnowledgeBase{},
		&types.Knowledge{},
		&types.TaskPendingOp{},
	))
	require.NoError(t, db.Create(&types.Tenant{ID: 7, Name: "tenant"}).Error)
	require.NoError(t, db.Create(&types.KnowledgeBase{
		ID: "kb-1", TenantID: 7, Name: "knowledge base",
	}).Error)
	return db
}

func createMigrationCandidate(
	t *testing.T,
	db *gorm.DB,
	path string,
	content []byte,
	hashOverride string,
) *types.Knowledge {
	t.Helper()
	digest := md5.Sum(content)
	fileHash := hex.EncodeToString(digest[:])
	if hashOverride != "" {
		fileHash = hashOverride
	}
	knowledge := &types.Knowledge{
		ID:              "knowledge-1",
		TenantID:        7,
		KnowledgeBaseID: "kb-1",
		Type:            "file",
		ParseStatus:     types.ParseStatusCompleted,
		FileName:        "source.pdf",
		FileType:        "pdf",
		FileSize:        int64(len(content)),
		FileHash:        fileHash,
		FilePath:        path,
	}
	require.NoError(t, db.Create(knowledge).Error)
	return knowledge
}

func TestApplyRehomesSourceAndRetainsLegacyObject(t *testing.T) {
	db := openMigrationTestDB(t)
	store := newMemoryOBS(t)
	content := []byte("verified legacy source")
	legacyPath := "obs://bucket-a/legacy/source.pdf"
	store.objects[legacyPath] = append([]byte(nil), content...)
	createMigrationCandidate(t, db, legacyPath, content, "")

	migrator, err := newMigrator(
		db,
		ModeApply,
		t.TempDir(),
		store,
		store.binding,
	)
	require.NoError(t, err)
	report, err := migrator.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.CandidateDocuments)
	require.Equal(t, 1, report.SourceVerified)
	require.Equal(t, 1, report.UploadedObjects)
	require.Equal(t, 1, report.PathsSwitched)
	require.Equal(t, 1, report.LedgersCreated)
	require.Equal(t, 1, report.AssignedGenerations)
	require.Zero(t, report.RemainingCandidates)
	require.True(t, report.SourceObjectsRetained)

	var knowledge types.Knowledge
	require.NoError(t, db.First(&knowledge, "id = ?", "knowledge-1").Error)
	require.NotEqual(t, legacyPath, knowledge.FilePath)
	require.Contains(t, knowledge.FilePath, "/private/deployment-a/7/knowledge-1/")
	require.NotEmpty(t, knowledge.ProcessingGeneration)
	require.Equal(t, content, store.objects[legacyPath])
	require.Equal(t, content, store.objects[knowledge.FilePath])
	digest := sha256.Sum256(content)
	require.Equal(t, hex.EncodeToString(digest[:]), store.metadata[knowledge.FilePath])

	var rows []types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", knowledgeaux.TaskType).Find(&rows).Error)
	require.Len(t, rows, 1)
}

func TestAuditVerifiesSourceWithoutMutation(t *testing.T) {
	db := openMigrationTestDB(t)
	store := newMemoryOBS(t)
	content := []byte("audit-only source")
	legacyPath := "obs://bucket-a/legacy/source.pdf"
	store.objects[legacyPath] = append([]byte(nil), content...)
	createMigrationCandidate(t, db, legacyPath, content, "")

	migrator, err := newMigrator(
		db,
		ModeAudit,
		t.TempDir(),
		store,
		store.binding,
	)
	require.NoError(t, err)
	report, err := migrator.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.SourceVerified)
	require.Zero(t, report.UploadedObjects)
	require.Zero(t, report.LedgersCreated)
	require.Equal(t, 1, report.RemainingCandidates)

	var knowledge types.Knowledge
	require.NoError(t, db.First(&knowledge, "id = ?", "knowledge-1").Error)
	require.Equal(t, legacyPath, knowledge.FilePath)
	var ledgerCount int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Count(&ledgerCount).Error)
	require.Zero(t, ledgerCount)
}

func TestApplyBackfillsCanonicalPathWithoutReuploadAndIsIdempotent(t *testing.T) {
	db := openMigrationTestDB(t)
	store := newMemoryOBS(t)
	content := []byte("canonical source without ledger")
	currentPath := "obs://bucket-a/private/deployment-a/7/knowledge-1/source.pdf"
	store.objects[currentPath] = append([]byte(nil), content...)
	createMigrationCandidate(t, db, currentPath, content, "")

	migrator, err := newMigrator(
		db,
		ModeApply,
		t.TempDir(),
		store,
		store.binding,
	)
	require.NoError(t, err)
	report, err := migrator.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.LedgerOnlyCandidates)
	require.Zero(t, report.RehomeCandidates)
	require.Zero(t, report.UploadedObjects)
	require.Zero(t, report.PathsSwitched)
	require.Equal(t, 1, report.LedgersCreated)
	require.Zero(t, report.RemainingCandidates)
	require.Equal(t, content, store.objects[currentPath])
	require.Empty(t, store.metadata[currentPath])

	retryReport, err := migrator.Run(context.Background())
	require.NoError(t, err)
	require.Zero(t, retryReport.CandidateDocuments)
	require.Zero(t, retryReport.LedgersCreated)
	require.Zero(t, retryReport.RemainingCandidates)
}

func TestApplyRejectsHashMismatchWithoutCutover(t *testing.T) {
	db := openMigrationTestDB(t)
	store := newMemoryOBS(t)
	content := []byte("tampered legacy source")
	legacyPath := "obs://bucket-a/legacy/source.pdf"
	store.objects[legacyPath] = append([]byte(nil), content...)
	createMigrationCandidate(t, db, legacyPath, content, fmt.Sprintf("%032x", 1))

	migrator, err := newMigrator(
		db,
		ModeApply,
		t.TempDir(),
		store,
		store.binding,
	)
	require.NoError(t, err)
	report, err := migrator.Run(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, report.FailedDocuments)
	require.Equal(t, 1, report.RemainingCandidates)
	require.Zero(t, report.UploadedObjects)
	require.Zero(t, report.LedgersCreated)

	var knowledge types.Knowledge
	require.NoError(t, db.First(&knowledge, "id = ?", "knowledge-1").Error)
	require.Equal(t, legacyPath, knowledge.FilePath)
	var ledgerCount int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Count(&ledgerCount).Error)
	require.Zero(t, ledgerCount)
}

package mobiledocument

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/artifactstore"
	"gorm.io/gorm"
)

const artifactStorageStateReady = "ready"

// ArtifactFile is the minimal persisted artifact contract required by the
// mobile capability-download boundary. The handler only returns its streamed
// content and download metadata; the private object key never enters an API
// response.
type ArtifactFile struct {
	ID           string         `gorm:"column:id;primaryKey"`
	TenantID     uint64         `gorm:"column:tenant_id"`
	UserID       string         `gorm:"column:user_id"`
	FilePath     string         `gorm:"column:file_path"`
	StorageState string         `gorm:"column:storage_state"`
	FileName     string         `gorm:"column:file_name"`
	FileType     string         `gorm:"column:file_type"`
	FileSize     int64          `gorm:"column:file_size"`
	ContentType  string         `gorm:"column:content_type"`
	DeletedAt    gorm.DeletedAt `gorm:"column:deleted_at"`
}

func (ArtifactFile) TableName() string { return "custom_general_agent_artifacts" }

type artifactFiles interface {
	GetArtifact(
		ctx context.Context,
		id string,
		tenantID uint64,
		userID string,
	) (*ArtifactFile, error)
	OpenArtifact(ctx context.Context, file *ArtifactFile) (io.ReadCloser, error)
}

type ArtifactRepository struct {
	db    *gorm.DB
	store *artifactstore.Store
}

func NewArtifactRepository(db *gorm.DB, store *artifactstore.Store) *ArtifactRepository {
	return &ArtifactRepository{db: db, store: store}
}

func (r *ArtifactRepository) GetArtifact(
	ctx context.Context,
	id string,
	tenantID uint64,
	userID string,
) (*ArtifactFile, error) {
	id = strings.TrimSpace(id)
	userID = strings.TrimSpace(userID)
	if r == nil || r.db == nil || id == "" || tenantID == 0 || userID == "" {
		return nil, ErrArtifactUnavailable
	}

	var file ArtifactFile
	err := r.db.WithContext(ctx).
		Where(
			"id = ? AND tenant_id = ? AND user_id = ? AND storage_state = ?",
			id,
			tenantID,
			userID,
			artifactStorageStateReady,
		).
		First(&file).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrArtifactUnavailable
	}
	if err != nil {
		return nil, err
	}
	return &file, nil
}

func (r *ArtifactRepository) OpenArtifact(
	ctx context.Context,
	file *ArtifactFile,
) (io.ReadCloser, error) {
	if r == nil || r.store == nil || file == nil || strings.TrimSpace(file.FilePath) == "" {
		return nil, ErrArtifactUnavailable
	}
	reader, err := r.store.Open(ctx, file.FilePath)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

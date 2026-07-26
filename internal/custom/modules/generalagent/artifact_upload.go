package generalagent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const artifactMetadataHeader = "X-WeKnora-Artifact-Metadata"

var artifactSHA256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type ArtifactUploadMetadata struct {
	TenantID           uint64 `json:"tenant_id"`
	UserID             string `json:"user_id"`
	SessionID          string `json:"session_id"`
	RunID              string `json:"run_id"`
	AssistantMessageID string `json:"assistant_message_id"`
	FileToken          string `json:"file_token"`
	FileName           string `json:"filename"`
	FileType           string `json:"file_type"`
	FileSize           int64  `json:"file_size"`
	SHA256             string `json:"sha256"`
	ContentType        string `json:"content_type"`
}

func (h *Handler) UploadArtifact(c *gin.Context) {
	if !validInternalAPIKey(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if h == nil || h.service == nil || h.service.db == nil || h.service.artifactStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "private artifact storage unavailable"})
		return
	}
	meta, err := decodeArtifactUploadMetadata(c.GetHeader(artifactMetadataHeader))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	maxBytes := artifactUploadMaxBytes()
	if c.Request.ContentLength < 0 || c.Request.ContentLength >= maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "artifact body exceeds upload limit"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBytes))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read artifact body"})
		return
	}
	if int64(len(data)) >= maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "artifact body exceeds upload limit"})
		return
	}
	result, err := h.service.stageArtifact(c.Request.Context(), meta, data)
	if err != nil {
		logger.Warnf(c.Request.Context(),
			"[general-agent-artifact] upload rejected tenant=%d session=%s run=%s token=%s: %v",
			meta.TenantID, meta.SessionID, meta.RunID, meta.FileToken, err,
		)
		c.JSON(http.StatusConflict, gin.H{"error": "artifact upload could not be committed"})
		return
	}
	c.JSON(http.StatusOK, result)
}

func decodeArtifactUploadMetadata(raw string) (ArtifactUploadMetadata, error) {
	var meta ArtifactUploadMetadata
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return meta, errors.New("artifact metadata is required")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return meta, errors.New("invalid artifact metadata encoding")
	}
	if len(data) > 16*1024 {
		return meta, errors.New("artifact metadata is too large")
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, errors.New("invalid artifact metadata")
	}
	return meta, nil
}

func artifactUploadMaxBytes() int64 {
	mb := envInt("CUSTOM_GENERAL_AGENT_ARTIFACT_UPLOAD_MAX_MB", 128)
	return int64(mb) * 1024 * 1024
}

func (s *Service) stageArtifact(
	ctx context.Context,
	meta ArtifactUploadMetadata,
	data []byte,
) (*SidecarArtifact, error) {
	if s == nil || s.db == nil || s.artifactStore == nil {
		return nil, errors.New("artifact storage is unavailable")
	}
	meta = normalizeArtifactUploadMetadata(meta)
	if err := validateArtifactUpload(meta, data); err != nil {
		return nil, err
	}
	if err := s.validateArtifactSession(ctx, meta); err != nil {
		return nil, err
	}
	row, err := s.reserveArtifactRow(ctx, meta)
	if err != nil {
		return nil, err
	}

	if row.StorageState == artifactStorageStateReady {
		if err := s.artifactStore.Verify(ctx, row.FilePath, row.FileSize, row.SHA256); err == nil {
			return sidecarArtifactFromRow(row), nil
		}
		// The row is authoritative but the remote commit is absent or uncertain.
		// Move it back to the retryable state and overwrite the exact same key.
		if err := s.db.WithContext(ctx).Model(&Artifact{}).
			Where("id = ? AND storage_state = ?", row.ID, artifactStorageStateReady).
			Update("storage_state", artifactStorageStateUploading).Error; err != nil {
			return nil, err
		}
		row.StorageState = artifactStorageStateUploading
	}

	if err := s.artifactStore.CommitAndVerify(
		ctx,
		data,
		row.FilePath,
		row.ContentType,
		row.SHA256,
	); err != nil {
		return nil, err
	}
	result := s.db.WithContext(ctx).Model(&Artifact{}).
		Where(
			"id = ? AND tenant_id = ? AND run_id = ? AND file_token = ? AND storage_state = ?",
			row.ID,
			row.TenantID,
			row.RunID,
			row.FileToken,
			artifactStorageStateUploading,
		).
		Where(
			"EXISTS (SELECT 1 FROM sessions WHERE sessions.id = ? AND sessions.tenant_id = ? AND sessions.user_id = ? AND sessions.deleted_at IS NULL)",
			row.SessionID,
			row.TenantID,
			row.UserID,
		).
		Update("storage_state", artifactStorageStateReady)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		// A retry may have reached another app replica at the same time. Both
		// replicas write the exact same bytes to the deterministic key; if the
		// peer already won the ready transition, verification makes this
		// request successful too.
		var current Artifact
		if err := s.db.WithContext(ctx).First(&current, "id = ?", row.ID).Error; err == nil &&
			current.StorageState == artifactStorageStateReady {
			if err := s.artifactStore.Verify(ctx, current.FilePath, current.FileSize, current.SHA256); err == nil {
				return sidecarArtifactFromRow(&current), nil
			}
		}
		// The session or reservation was deleted while remote I/O was in
		// flight. Compensate the just-finished object write so no orphan is
		// left behind.
		if err := s.artifactStore.Delete(ctx, row.FilePath); err != nil && !legacyArtifactNotFound(err) {
			return nil, errors.Join(
				errors.New("artifact upload reservation was lost"),
				fmt.Errorf("compensating object cleanup failed: %w", err),
			)
		}
		return nil, errors.New("artifact upload reservation was lost")
	}
	row.StorageState = artifactStorageStateReady
	return sidecarArtifactFromRow(row), nil
}

func validateArtifactUpload(meta ArtifactUploadMetadata, data []byte) error {
	if meta.TenantID == 0 || meta.UserID == "" || meta.SessionID == "" || meta.RunID == "" ||
		meta.AssistantMessageID == "" || meta.FileToken == "" || meta.FileName == "" {
		return errors.New("artifact identity is incomplete")
	}
	for _, value := range []string{meta.SessionID, meta.RunID, meta.AssistantMessageID, meta.FileToken} {
		if strings.ContainsAny(value, `/\`) || strings.Contains(value, "..") || len(value) > 255 {
			return errors.New("artifact identity is invalid")
		}
	}
	if safeFileName(meta.FileName) != meta.FileName || len([]rune(meta.FileName)) > 255 {
		return errors.New("artifact filename is invalid")
	}
	if meta.FileSize < 0 || meta.FileSize != int64(len(data)) {
		return errors.New("artifact size mismatch")
	}
	if !artifactSHA256Pattern.MatchString(meta.SHA256) {
		return errors.New("artifact sha256 is invalid")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != meta.SHA256 {
		return errors.New("artifact sha256 mismatch")
	}
	return nil
}

func normalizeArtifactUploadMetadata(meta ArtifactUploadMetadata) ArtifactUploadMetadata {
	meta.UserID = strings.TrimSpace(meta.UserID)
	meta.SessionID = strings.TrimSpace(meta.SessionID)
	meta.RunID = strings.TrimSpace(meta.RunID)
	meta.AssistantMessageID = strings.TrimSpace(meta.AssistantMessageID)
	meta.FileToken = strings.TrimSpace(meta.FileToken)
	meta.FileName = strings.TrimSpace(meta.FileName)
	meta.FileType = strings.TrimSpace(meta.FileType)
	meta.SHA256 = strings.ToLower(strings.TrimSpace(meta.SHA256))
	meta.ContentType = strings.TrimSpace(meta.ContentType)
	return meta
}

func (s *Service) validateArtifactSession(ctx context.Context, meta ArtifactUploadMetadata) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&types.Session{}).
		Where(
			"id = ? AND tenant_id = ? AND user_id = ? AND deleted_at IS NULL",
			meta.SessionID,
			meta.TenantID,
			meta.UserID,
		).
		Count(&count).Error; err != nil {
		return fmt.Errorf("validate artifact session: %w", err)
	}
	if count != 1 {
		return errors.New("artifact session is missing or no longer active")
	}
	return nil
}

func (s *Service) reserveArtifactRow(
	ctx context.Context,
	meta ArtifactUploadMetadata,
) (*Artifact, error) {
	var row Artifact
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		loadReservation := func() error {
			return tx.Session(&gorm.Session{NewDB: true}).Where(
				"tenant_id = ? AND run_id = ? AND file_token = ?",
				meta.TenantID,
				meta.RunID,
				meta.FileToken,
			).First(&row).Error
		}
		err := loadReservation()
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			artifactID := uuid.NewString()
			filePath, reserveErr := s.artifactStore.Reserve(
				meta.TenantID,
				meta.SessionID,
				meta.RunID,
				artifactID,
				meta.FileName,
			)
			if reserveErr != nil {
				return reserveErr
			}
			contentType := strings.TrimSpace(meta.ContentType)
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			candidate := Artifact{
				ID:           artifactID,
				TenantID:     meta.TenantID,
				UserID:       strings.TrimSpace(meta.UserID),
				RunID:        strings.TrimSpace(meta.RunID),
				SessionID:    strings.TrimSpace(meta.SessionID),
				MessageID:    strings.TrimSpace(meta.AssistantMessageID),
				FileToken:    strings.TrimSpace(meta.FileToken),
				FilePath:     filePath,
				StorageState: artifactStorageStateUploading,
				FileName:     strings.TrimSpace(meta.FileName),
				FileType:     strings.TrimPrefix(strings.ToLower(strings.TrimSpace(meta.FileType)), "."),
				FileSize:     meta.FileSize,
				SHA256:       strings.ToLower(strings.TrimSpace(meta.SHA256)),
				ContentType:  contentType,
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; err != nil {
				return err
			}
			// Never reuse the statement that returned ErrRecordNotFound.
			// GORM retains destination/query state on reused handles and could
			// otherwise make a successful reservation look absent.
			if err := loadReservation(); err != nil {
				return err
			}
		}
		return validateArtifactReservation(row, meta)
	})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func validateArtifactReservation(row Artifact, meta ArtifactUploadMetadata) error {
	if row.DeletedAt.Valid ||
		row.TenantID != meta.TenantID ||
		row.UserID != strings.TrimSpace(meta.UserID) ||
		row.SessionID != strings.TrimSpace(meta.SessionID) ||
		row.RunID != strings.TrimSpace(meta.RunID) ||
		row.MessageID != strings.TrimSpace(meta.AssistantMessageID) ||
		row.FileToken != strings.TrimSpace(meta.FileToken) ||
		row.FileName != strings.TrimSpace(meta.FileName) ||
		row.FileSize != meta.FileSize ||
		!strings.EqualFold(row.SHA256, strings.TrimSpace(meta.SHA256)) {
		return errors.New("artifact idempotency key was reused with different metadata")
	}
	if row.StorageState != artifactStorageStateUploading && row.StorageState != artifactStorageStateReady {
		return fmt.Errorf("artifact has invalid storage state %q", row.StorageState)
	}
	return nil
}

func sidecarArtifactFromRow(row *Artifact) *SidecarArtifact {
	if row == nil {
		return nil
	}
	return &SidecarArtifact{
		FileToken:   row.FileToken,
		FileName:    row.FileName,
		FileType:    row.FileType,
		FileSize:    row.FileSize,
		SHA256:      row.SHA256,
		ContentType: row.ContentType,
		ArtifactID:  row.ID,
		DownloadURL: "/api/v1/custom/general-agent/artifacts/" + row.ID + "/download",
		Persisted:   row.StorageState == artifactStorageStateReady,
	}
}

func encodeArtifactUploadMetadata(meta ArtifactUploadMetadata) string {
	data, _ := json.Marshal(meta)
	return base64.RawURLEncoding.EncodeToString(data)
}

func artifactContentLengthHeader(size int64) string {
	return strconv.FormatInt(size, 10)
}

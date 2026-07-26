package generalagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

const artifactMigrationAdvisoryLock int64 = 0x574B4E4F524141 // "WKNORAA"

// migrateLegacyArtifacts converts historical app-local artifact paths before
// the app starts accepting traffic. A PostgreSQL session lock serializes the
// operation across replicas, so only the replica that can see the legacy
// migration volume performs the copy; later replicas observe object URIs.
func (s *Service) migrateLegacyArtifacts(ctx context.Context) error {
	if s == nil || s.db == nil || s.artifactStore == nil {
		return errors.New("legacy artifact migration dependencies are unavailable")
	}
	run := func(db *gorm.DB) error {
		if db.Dialector.Name() == "postgres" {
			if err := freshArtifactMigrationDB(ctx, db).
				Exec("SELECT pg_advisory_lock(?)", artifactMigrationAdvisoryLock).Error; err != nil {
				return fmt.Errorf("lock legacy artifact migration: %w", err)
			}
			defer func() {
				if err := freshArtifactMigrationDB(ctx, db).
					Exec("SELECT pg_advisory_unlock(?)", artifactMigrationAdvisoryLock).Error; err != nil {
					logger.Warnf(ctx, "[general-agent-artifact] failed to release migration lock: %v", err)
				}
			}()
		}
		return s.migrateLegacyArtifactRows(ctx, db)
	}
	if s.db.Dialector.Name() == "postgres" {
		return s.db.WithContext(ctx).Connection(run)
	}
	return run(s.db.WithContext(ctx))
}

func (s *Service) migrateLegacyArtifactRows(ctx context.Context, db *gorm.DB) error {
	var rows []Artifact
	if err := freshArtifactMigrationDB(ctx, db).
		Where("deleted_at IS NULL").
		Order("created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return fmt.Errorf("list legacy artifacts: %w", err)
	}
	migrated := 0
	for i := range rows {
		row := &rows[i]
		if s.artifactStore.Owns(row.FilePath) {
			if row.StorageState != artifactStorageStateReady {
				if err := s.artifactStore.Verify(ctx, row.FilePath, row.FileSize, row.SHA256); err != nil {
					return fmt.Errorf("verify interrupted artifact migration %s: %w", row.ID, err)
				}
				if err := freshArtifactMigrationDB(ctx, db).Model(&Artifact{}).
					Where("id = ?", row.ID).
					Update("storage_state", artifactStorageStateReady).Error; err != nil {
					return err
				}
			}
			continue
		}
		// A prior replica may already have inspected and explicitly
		// quarantined a pre-existing bad legacy row. Later replicas must not
		// require that replica's one-time legacy volume merely to repeat the
		// same diagnosis. Production defaults keep both allowances disabled.
		if row.StorageState == artifactStorageStateMissing && allowMissingLegacyArtifacts() {
			continue
		}
		if row.StorageState == artifactStorageStateCorrupt && allowInvalidLegacyArtifacts() {
			continue
		}
		data, err := s.readLegacyArtifact(ctx, row.FilePath)
		if err != nil {
			if allowMissingLegacyArtifacts() && legacyArtifactNotFound(err) {
				if updateErr := freshArtifactMigrationDB(ctx, db).Model(&Artifact{}).
					Where("id = ? AND file_path = ?", row.ID, row.FilePath).
					Update("storage_state", artifactStorageStateMissing).Error; updateErr != nil {
					return updateErr
				}
				logger.Warnf(ctx,
					"[general-agent-artifact] quarantined pre-existing missing legacy artifact id=%s tenant=%d",
					row.ID,
					row.TenantID,
				)
				continue
			}
			return fmt.Errorf("read legacy artifact %s: %w", row.ID, err)
		}
		sum := sha256.Sum256(data)
		actualSHA := hex.EncodeToString(sum[:])
		if int64(len(data)) != row.FileSize || !strings.EqualFold(actualSHA, row.SHA256) {
			if allowInvalidLegacyArtifacts() {
				if updateErr := freshArtifactMigrationDB(ctx, db).Model(&Artifact{}).
					Where("id = ? AND file_path = ?", row.ID, row.FilePath).
					Update("storage_state", artifactStorageStateCorrupt).Error; updateErr != nil {
					return updateErr
				}
				logger.Warnf(ctx,
					"[general-agent-artifact] quarantined pre-existing corrupt legacy artifact id=%s tenant=%d",
					row.ID,
					row.TenantID,
				)
				continue
			}
			return fmt.Errorf("legacy artifact %s failed size/sha256 verification", row.ID)
		}
		target, err := s.artifactStore.Reserve(
			row.TenantID,
			row.SessionID,
			row.RunID,
			row.ID,
			row.FileName,
		)
		if err != nil {
			return fmt.Errorf("reserve migrated artifact %s: %w", row.ID, err)
		}
		if err := freshArtifactMigrationDB(ctx, db).Model(&Artifact{}).
			Where("id = ? AND file_path = ?", row.ID, row.FilePath).
			Update("storage_state", artifactStorageStateUploading).Error; err != nil {
			return err
		}
		if err := s.artifactStore.CommitAndVerify(
			ctx,
			data,
			target,
			row.ContentType,
			row.SHA256,
		); err != nil {
			return fmt.Errorf("commit migrated artifact %s: %w", row.ID, err)
		}
		result := freshArtifactMigrationDB(ctx, db).Model(&Artifact{}).
			Where("id = ? AND file_path = ?", row.ID, row.FilePath).
			Updates(map[string]interface{}{
				"file_path":     target,
				"storage_state": artifactStorageStateReady,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			var current Artifact
			if err := freshArtifactMigrationDB(ctx, db).First(&current, "id = ?", row.ID).Error; err != nil {
				return err
			}
			if !s.artifactStore.Owns(current.FilePath) {
				return fmt.Errorf("legacy artifact %s migration lost its database reservation", row.ID)
			}
		}
		migrated++
	}
	if migrated > 0 {
		logger.Infof(ctx, "[general-agent-artifact] migrated and verified %d legacy local artifacts into private %s objects",
			migrated, s.artifactStore.Provider())
	}
	return nil
}

func freshArtifactMigrationDB(ctx context.Context, db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{NewDB: true}).WithContext(ctx)
}

func allowMissingLegacyArtifacts() bool {
	return envBool("CUSTOM_GENERAL_AGENT_ARTIFACT_MIGRATION_ALLOW_MISSING", false)
}

func allowInvalidLegacyArtifacts() bool {
	return envBool("CUSTOM_GENERAL_AGENT_ARTIFACT_MIGRATION_ALLOW_INVALID", false)
}

func legacyArtifactNotFound(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such file") ||
		strings.Contains(message, "not found") ||
		strings.Contains(message, "nosuchkey") ||
		strings.Contains(message, "specified key does not exist")
}

func (s *Service) readLegacyArtifact(ctx context.Context, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "local://") {
		if s.fileService == nil {
			return nil, errors.New("legacy local file service is unavailable")
		}
		reader, err := s.fileService.GetFile(ctx, raw)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(io.LimitReader(reader, artifactUploadMaxBytes()+1))
	}
	if strings.HasPrefix(raw, "minio://") || strings.HasPrefix(raw, "obs://") {
		service, err := legacyArtifactObjectService(raw)
		if err != nil {
			return nil, err
		}
		reader, err := service.GetFile(ctx, raw)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(io.LimitReader(reader, artifactUploadMaxBytes()+1))
	}
	legacyPath, err := s.validatedLegacyArtifactPath(raw)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(legacyPath)
}

func legacyArtifactObjectService(filePath string) (interfaces.FileService, error) {
	parts := strings.SplitN(filePath, "://", 2)
	if len(parts) != 2 {
		return nil, errors.New("invalid legacy artifact object URI")
	}
	provider := strings.ToLower(parts[0])
	bucketAndKey := strings.SplitN(parts[1], "/", 2)
	if len(bucketAndKey) != 2 || strings.TrimSpace(bucketAndKey[0]) == "" || strings.TrimSpace(bucketAndKey[1]) == "" {
		return nil, errors.New("invalid legacy artifact object URI")
	}
	bucket := strings.TrimSpace(bucketAndKey[0])
	switch provider {
	case "minio":
		endpoint := artifactMigrationEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_MINIO_ENDPOINT",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_ENDPOINT",
			"MINIO_ENDPOINT",
		)
		accessKey := artifactMigrationEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_MINIO_ACCESS_KEY_ID",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_ACCESS_KEY_ID",
			"MINIO_ACCESS_KEY_ID",
		)
		secretKey := artifactMigrationEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_MINIO_SECRET_ACCESS_KEY",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_MINIO_SECRET_ACCESS_KEY",
			"MINIO_SECRET_ACCESS_KEY",
		)
		if endpoint == "" || accessKey == "" || secretKey == "" {
			return nil, errors.New("legacy MinIO artifact credentials are incomplete")
		}
		return filesvc.NewMinioFileServiceWithPathPrefix(
			endpoint,
			accessKey,
			secretKey,
			bucket,
			envBool("CUSTOM_GENERAL_AGENT_ARTIFACT_MINIO_USE_SSL", false),
			"",
		)
	case "obs":
		endpoint := artifactMigrationEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_OBS_ENDPOINT",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_ENDPOINT",
			"OBS_ENDPOINT",
		)
		region := artifactMigrationEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_OBS_REGION",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_REGION",
			"OBS_REGION",
		)
		if region == "" {
			region = "cn-north-4"
		}
		accessKey := artifactMigrationEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_OBS_ACCESS_KEY",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_ACCESS_KEY",
			"OBS_ACCESS_KEY",
		)
		secretKey := artifactMigrationEnv(
			"CUSTOM_GENERAL_AGENT_ARTIFACT_OBS_SECRET_KEY",
			"CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_OBS_SECRET_KEY",
			"OBS_SECRET_KEY",
		)
		if endpoint == "" || accessKey == "" || secretKey == "" {
			return nil, errors.New("legacy OBS artifact credentials are incomplete")
		}
		return filesvc.NewObsFileService(endpoint, region, accessKey, secretKey, bucket, "")
	default:
		return nil, fmt.Errorf("unsupported legacy artifact provider %q", provider)
	}
}

func artifactMigrationEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func (s *Service) validatedLegacyArtifactPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return "", errors.New("legacy path is not a local file")
	}
	candidate := raw
	absolute, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	for _, root := range s.legacyArtifactRoots() {
		relative, relErr := filepath.Rel(root, absolute)
		if relErr == nil && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
			relative != ".." && !filepath.IsAbs(relative) {
			return absolute, nil
		}
	}
	return "", errors.New("legacy path is outside an approved artifact root")
}

func (s *Service) legacyArtifactRoots() []string {
	rawRoots := []string{
		s.artifactRoot,
		"/data/files/general-agent-artifacts",
	}
	for _, value := range strings.Split(os.Getenv("CUSTOM_GENERAL_AGENT_ARTIFACT_LEGACY_ROOTS"), ",") {
		if strings.TrimSpace(value) != "" {
			rawRoots = append(rawRoots, strings.TrimSpace(value))
		}
	}
	roots := make([]string, 0, len(rawRoots))
	seen := make(map[string]struct{}, len(rawRoots))
	for _, raw := range rawRoots {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		absolute, err := filepath.Abs(filepath.Clean(raw))
		if err != nil {
			continue
		}
		if _, ok := seen[absolute]; ok {
			continue
		}
		seen[absolute] = struct{}{}
		roots = append(roots, absolute)
	}
	return roots
}

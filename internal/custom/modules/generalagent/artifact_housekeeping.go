package generalagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

func (s *Service) StartArtifactHousekeeping() {
	if s == nil {
		return
	}
	s.housekeepingOnce.Do(func() {
		go func() {
			ctx := context.Background()
			s.runArtifactHousekeeping(ctx)
			interval := time.Duration(envInt("CUSTOM_GENERAL_AGENT_ARTIFACT_HOUSEKEEPING_SECONDS", 60)) * time.Second
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for range ticker.C {
				s.runArtifactHousekeeping(ctx)
			}
		}()
	})
}

func (s *Service) runArtifactHousekeeping(ctx context.Context) {
	if s == nil || s.db == nil || s.artifactStore == nil {
		return
	}
	if err := s.cleanupOrphanArtifacts(ctx); err != nil {
		logger.Warnf(ctx, "[general-agent-artifact] orphan cleanup failed and will retry: %v", err)
	}
	if err := s.cleanupStaleArtifactUploads(ctx); err != nil {
		logger.Warnf(ctx, "[general-agent-artifact] stale upload cleanup failed and will retry: %v", err)
	}
}

func (s *Service) DeleteSessionArtifacts(
	ctx context.Context,
	tenantID uint64,
	userID string,
	sessionIDs []string,
) error {
	if s == nil || s.db == nil || tenantID == 0 || len(sessionIDs) == 0 {
		return nil
	}
	query := s.db.WithContext(ctx).
		Where("tenant_id = ? AND session_id IN ?", tenantID, sessionIDs)
	if strings.TrimSpace(userID) != "" {
		query = query.Where("user_id = ?", strings.TrimSpace(userID))
	}
	var rows []Artifact
	if err := query.Find(&rows).Error; err != nil {
		return err
	}
	return s.deleteArtifactRows(ctx, rows, false)
}

func (s *Service) cleanupOrphanArtifacts(ctx context.Context) error {
	var rows []Artifact
	err := s.db.WithContext(ctx).
		Raw(`
			SELECT a.*
			FROM custom_general_agent_artifacts a
			LEFT JOIN sessions s
			  ON s.id = a.session_id
			 AND s.tenant_id = a.tenant_id
			WHERE a.deleted_at IS NULL
			  AND (s.id IS NULL OR s.deleted_at IS NOT NULL)
			ORDER BY a.created_at ASC
			LIMIT 100
		`).
		Scan(&rows).Error
	if err != nil {
		return err
	}
	return s.deleteArtifactRows(ctx, rows, false)
}

func (s *Service) cleanupStaleArtifactUploads(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-time.Duration(
		envInt("CUSTOM_GENERAL_AGENT_ARTIFACT_STALE_UPLOAD_MINUTES", 60),
	) * time.Minute)
	var rows []Artifact
	if err := s.db.WithContext(ctx).
		Where("storage_state = ? AND created_at < ?", artifactStorageStateUploading, cutoff).
		Order("created_at ASC").
		Limit(100).
		Find(&rows).Error; err != nil {
		return err
	}
	return s.deleteArtifactRows(ctx, rows, true)
}

func (s *Service) deleteArtifactRows(ctx context.Context, rows []Artifact, forceUploading bool) error {
	var errs []error
	for i := range rows {
		row := &rows[i]
		// A regular session/orphan sweep must not race an active remote write.
		// The ready transition is followed by the next orphan sweep; a failed
		// write is handled by the explicitly stale-upload sweep.
		if row.StorageState == artifactStorageStateUploading && !forceUploading {
			continue
		}
		if err := s.deleteArtifactPhysical(ctx, row.FilePath); err != nil {
			errs = append(errs, fmt.Errorf("artifact %s: %w", row.ID, err))
			continue
		}
		if err := s.db.WithContext(ctx).
			Where("id = ? AND deleted_at IS NULL", row.ID).
			Delete(&Artifact{}).Error; err != nil {
			errs = append(errs, fmt.Errorf("soft-delete artifact %s: %w", row.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) deleteArtifactPhysical(ctx context.Context, filePath string) error {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return nil
	}
	var err error
	switch {
	case s.artifactStore != nil && s.artifactStore.Owns(filePath):
		err = s.artifactStore.Delete(ctx, filePath)
	case strings.HasPrefix(filePath, "local://"):
		if s.fileService == nil {
			return errors.New("legacy local file service is unavailable")
		}
		err = s.fileService.DeleteFile(ctx, filePath)
	case strings.HasPrefix(filePath, "minio://"), strings.HasPrefix(filePath, "obs://"):
		var serviceErr error
		service, serviceErr := legacyArtifactObjectService(filePath)
		if serviceErr != nil {
			return serviceErr
		}
		err = service.DeleteFile(ctx, filePath)
	default:
		localPath, pathErr := s.validatedLegacyArtifactPath(filePath)
		if pathErr != nil {
			return pathErr
		}
		err = os.Remove(localPath)
	}
	if err != nil && !os.IsNotExist(err) && !legacyArtifactNotFound(err) {
		return err
	}
	return nil
}

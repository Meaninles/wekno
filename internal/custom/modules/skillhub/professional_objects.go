package skillhub

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type canonicalProfessionalArchive struct {
	Data      []byte
	SHA256    string
	FileCount int
}

func (s *Service) ListProfessionalForManage(
	ctx context.Context,
) ([]ProfessionalSkillListItem, error) {
	reserved, err := discoverReservedProfessionalMetadata()
	if err != nil {
		return nil, err
	}
	access, err := s.professionalAccessByName(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProfessionalSkillListItem, 0, len(reserved)+len(access))
	for _, meta := range reserved {
		files, listErr := listProfessionalSkillFiles(meta.BasePath)
		if listErr != nil {
			return nil, listErr
		}
		files = filterRuntimeProfessionalFiles(files)
		out = append(out, ProfessionalSkillListItem{
			Name:           meta.Name,
			DisplayName:    meta.DisplayName,
			Description:    meta.Description,
			Kind:           "professional",
			FileCount:      len(files),
			Managed:        true,
			IsMine:         true,
			CanManage:      false,
			CanDownload:    false,
			SystemReserved: true,
			UpdatedAt:      professionalSkillUpdatedAt(meta.BasePath),
		})
	}
	for _, entry := range access {
		if !entry.Accessible || entry.Record == nil {
			continue
		}
		out = append(out, s.professionalItemFromAccess(
			entry,
			entry.Record.FileCount,
			timePointer(entry.Record.UpdatedAt),
		))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Service) ImportProfessionalSkill(
	ctx context.Context,
	req ProfessionalSkillImportRequest,
) (*ProfessionalSkillListItem, error) {
	if !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleContributor) {
		return nil, fmt.Errorf("permission denied")
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	if tenantID == 0 || userID == "" {
		return nil, fmt.Errorf("tenant and user are required")
	}
	if IsReservedProfessionalSkillName(req.Name) {
		return nil, fmt.Errorf(
			"professional skill %q is system reserved",
			strings.TrimSpace(req.Name),
		)
	}
	description, err := normalizeProfessionalDescription(req.Description)
	if err != nil {
		return nil, err
	}
	if req.File == nil {
		return nil, fmt.Errorf("professional skill package is required")
	}
	if err := s.requireProfessionalObjectStore(); err != nil {
		return nil, err
	}

	skillRoot, identity, cleanup, err := extractUploadedProfessionalSkill(
		req.File,
		req.Filename,
		req.Name,
		req.DisplayName,
		"weknora-professional-skill-*",
	)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if IsReservedProfessionalSkillName(identity.RuntimeName) {
		return nil, fmt.Errorf(
			"professional skill %q is system reserved",
			identity.RuntimeName,
		)
	}
	if description == "" {
		description = identity.Description
	}
	repairRecord, err := s.repairableProfessionalRecord(
		ctx,
		identity.RuntimeName,
		tenantID,
		userID,
	)
	if err != nil {
		return nil, err
	}
	archive, err := buildCanonicalProfessionalArchive(skillRoot)
	if err != nil {
		return nil, err
	}

	record := &ProfessionalSkill{
		ID:              uuid.NewString(),
		TenantID:        tenantID,
		CreatorID:       userID,
		Name:            identity.RuntimeName,
		DisplayName:     identity.DisplayName,
		Description:     description,
		ArchiveFileName: cleanProfessionalArchiveFilename(req.Filename),
		ObjectSize:      int64(len(archive.Data)),
		ObjectSHA256:    archive.SHA256,
		FileCount:       archive.FileCount,
	}
	if repairRecord != nil {
		record.ID = repairRecord.ID
		record.CreatorID = repairRecord.CreatorID
		record.CreatedAt = repairRecord.CreatedAt
	}
	objectPath, err := s.professionalStore.Reserve(
		record.TenantID,
		record.ID,
		uuid.NewString(),
	)
	if err != nil {
		return nil, err
	}
	record.ObjectPath = objectPath
	if err := s.professionalStore.CommitAndVerify(
		ctx,
		archive.Data,
		objectPath,
		"application/zip",
		archive.SHA256,
	); err != nil {
		return nil, fmt.Errorf("store professional skill package: %w", err)
	}
	if repairRecord != nil {
		now := time.Now()
		result := s.db.WithContext(ctx).
			Model(&ProfessionalSkill{}).
			Where("id = ? AND object_path = ''", repairRecord.ID).
			Updates(map[string]any{
				"display_name":      record.DisplayName,
				"description":       record.Description,
				"archive_file_name": record.ArchiveFileName,
				"object_path":       record.ObjectPath,
				"object_size":       record.ObjectSize,
				"object_sha256":     record.ObjectSHA256,
				"file_count":        record.FileCount,
				"updated_at":        now,
			})
		if result.Error != nil || result.RowsAffected != 1 {
			s.deleteUnreferencedProfessionalObject(ctx, objectPath)
			if result.Error != nil {
				return nil, result.Error
			}
			return nil, fmt.Errorf("professional skill was repaired concurrently; reload and retry")
		}
		record.UpdatedAt = now
	} else {
		if err := s.db.WithContext(ctx).Create(record).Error; err != nil {
			s.deleteUnreferencedProfessionalObject(ctx, objectPath)
			if isProfessionalSkillNameExistsError(err) {
				return nil, errProfessionalSkillNameExists
			}
			return nil, err
		}
	}
	item := s.professionalItemFromRecord(
		*record,
		true,
		"",
		"",
		record.FileCount,
		timePointer(record.UpdatedAt),
	)
	return &item, nil
}

func (s *Service) repairableProfessionalRecord(
	ctx context.Context,
	name string,
	tenantID uint64,
	userID string,
) (*ProfessionalSkill, error) {
	var existing ProfessionalSkill
	err := s.db.WithContext(ctx).First(&existing, "name = ?", strings.TrimSpace(name)).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if existing.ObjectPath != "" ||
		existing.TenantID != tenantID ||
		(existing.CreatorID != userID &&
			!types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin)) {
		return nil, errProfessionalSkillNameExists
	}
	return &existing, nil
}

func (s *Service) UpdateProfessionalSkill(
	ctx context.Context,
	id string,
	req ProfessionalSkillUpdateRequest,
) (*ProfessionalSkillListItem, error) {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return nil, err
	}
	if IsReservedProfessionalSkillName(record.Name) {
		return nil, fmt.Errorf(
			"professional skill %q is system reserved and cannot be modified",
			record.Name,
		)
	}
	nextName := strings.TrimSpace(req.Name)
	if nextName == "" {
		nextName = record.Name
	}
	if err := validateProfessionalSkillName(nextName); err != nil {
		return nil, err
	}
	if IsReservedProfessionalSkillName(nextName) {
		return nil, fmt.Errorf("professional skill %q is system reserved", nextName)
	}
	nextDisplayName := normalizeRequestedProfessionalDisplayName(req.DisplayName)
	if nextDisplayName == "" {
		nextDisplayName = record.DisplayName
	}
	if nextDisplayName == "" {
		nextDisplayName = nextName
	}
	nextDescription := record.Description
	if req.DescriptionProvided {
		nextDescription, err = normalizeProfessionalDescription(req.Description)
		if err != nil {
			return nil, err
		}
	}
	if nextName != record.Name {
		if err := s.ensureProfessionalSkillNameAvailable(ctx, nextName, record.ID); err != nil {
			return nil, err
		}
	}

	nextPath := record.ObjectPath
	nextSize := record.ObjectSize
	nextDigest := record.ObjectSHA256
	nextFileCount := record.FileCount
	nextArchiveName := record.ArchiveFileName
	var newObjectPath string
	if req.File != nil {
		if err := s.requireProfessionalObjectStore(); err != nil {
			return nil, err
		}
		skillRoot, identity, cleanup, extractErr := extractUploadedProfessionalSkill(
			req.File,
			req.Filename,
			nextName,
			nextDisplayName,
			"weknora-professional-skill-update-*",
		)
		if extractErr != nil {
			return nil, extractErr
		}
		defer cleanup()
		nextName = identity.RuntimeName
		nextDisplayName = identity.DisplayName
		if nextDescription == "" {
			nextDescription = identity.Description
		}
		archive, buildErr := buildCanonicalProfessionalArchive(skillRoot)
		if buildErr != nil {
			return nil, buildErr
		}
		newObjectPath, err = s.professionalStore.Reserve(
			record.TenantID,
			record.ID,
			uuid.NewString(),
		)
		if err != nil {
			return nil, err
		}
		if err := s.professionalStore.CommitAndVerify(
			ctx,
			archive.Data,
			newObjectPath,
			"application/zip",
			archive.SHA256,
		); err != nil {
			return nil, fmt.Errorf("store updated professional skill package: %w", err)
		}
		nextPath = newObjectPath
		nextSize = int64(len(archive.Data))
		nextDigest = archive.SHA256
		nextFileCount = archive.FileCount
		nextArchiveName = cleanProfessionalArchiveFilename(req.Filename)
	}

	now := time.Now()
	updates := map[string]any{
		"name":              nextName,
		"display_name":      nextDisplayName,
		"description":       nextDescription,
		"archive_file_name": nextArchiveName,
		"updated_at":        now,
	}
	if newObjectPath != "" {
		updates["object_path"] = nextPath
		updates["object_size"] = nextSize
		updates["object_sha256"] = nextDigest
		updates["file_count"] = nextFileCount
	}
	result := s.db.WithContext(ctx).Model(record).Updates(updates)
	if result.Error != nil {
		if newObjectPath != "" {
			s.deleteUnreferencedProfessionalObject(ctx, newObjectPath)
		}
		if isProfessionalSkillNameExistsError(result.Error) {
			return nil, errProfessionalSkillNameExists
		}
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		if newObjectPath != "" {
			s.deleteUnreferencedProfessionalObject(ctx, newObjectPath)
		}
		return nil, fmt.Errorf("professional skill changed concurrently; reload and retry")
	}
	oldObjectPath := record.ObjectPath
	record.Name = nextName
	record.DisplayName = nextDisplayName
	record.Description = nextDescription
	record.ArchiveFileName = nextArchiveName
	record.ObjectPath = nextPath
	record.ObjectSize = nextSize
	record.ObjectSHA256 = nextDigest
	record.FileCount = nextFileCount
	record.UpdatedAt = now
	if newObjectPath != "" && oldObjectPath != "" && oldObjectPath != newObjectPath {
		s.deleteUnreferencedProfessionalObject(ctx, oldObjectPath)
	}
	item := s.professionalItemFromRecord(
		*record,
		true,
		"",
		"",
		record.FileCount,
		timePointer(record.UpdatedAt),
	)
	return &item, nil
}

func (s *Service) DeleteProfessionalSkill(ctx context.Context, id string) error {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return err
	}
	if IsReservedProfessionalSkillName(record.Name) {
		return fmt.Errorf(
			"professional skill %q is system reserved and cannot be deleted",
			record.Name,
		)
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("skill_id = ?", record.ID).Delete(&OrganizationShare{}).Error; err != nil {
			return err
		}
		if err := tx.Where("skill_id = ?", record.ID).Delete(&UserShare{}).Error; err != nil {
			return err
		}
		return tx.Delete(record).Error
	}); err != nil {
		return err
	}
	if record.ObjectPath != "" && s.professionalStore != nil {
		if err := s.professionalStore.Delete(ctx, record.ObjectPath); err != nil {
			logger.Errorf(
				ctx,
				"[skillhub] deleted professional skill %s but failed to remove object %s: %v",
				record.ID,
				record.ObjectPath,
				err,
			)
		}
	}
	return nil
}

func (s *Service) DownloadProfessionalSkill(
	ctx context.Context,
	id string,
) (*ProfessionalSkillDownload, error) {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return nil, err
	}
	content, err := s.readProfessionalObject(ctx, record)
	if err != nil {
		return nil, err
	}
	return &ProfessionalSkillDownload{
		Filename: record.Name + ".zip",
		Reader:   io.NopCloser(bytes.NewReader(content)),
		Size:     int64(len(content)),
	}, nil
}

func (s *Service) requireProfessionalObjectStore() error {
	if s == nil || s.professionalStore == nil {
		if s != nil && s.professionalStoreErr != nil {
			return s.professionalStoreErr
		}
		return fmt.Errorf("professional skill object storage is unavailable")
	}
	return nil
}

func extractUploadedProfessionalSkill(
	file io.Reader,
	filename string,
	requestedName string,
	requestedDisplayName string,
	tempPattern string,
) (string, professionalSkillIdentity, func(), error) {
	archivePath, archiveCleanup, err := saveUploadedArchive(readerMultipartFile{Reader: file})
	if err != nil {
		return "", professionalSkillIdentity{}, nil, err
	}
	workDir, err := os.MkdirTemp("", tempPattern)
	if err != nil {
		archiveCleanup()
		return "", professionalSkillIdentity{}, nil, fmt.Errorf("create temp directory: %w", err)
	}
	cleanup := func() {
		archiveCleanup()
		_ = os.RemoveAll(workDir)
	}
	extractDir := filepath.Join(workDir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		cleanup()
		return "", professionalSkillIdentity{}, nil, err
	}
	if err := extractProfessionalSkillArchive(
		archivePath,
		filename,
		extractDir,
		time.Now().Add(professionalExtractTimeout),
	); err != nil {
		cleanup()
		return "", professionalSkillIdentity{}, nil, err
	}
	skillRoot, identity, err := validateExtractedProfessionalSkill(
		extractDir,
		requestedName,
		requestedDisplayName,
		filename,
	)
	if err != nil {
		cleanup()
		return "", professionalSkillIdentity{}, nil, err
	}
	return skillRoot, identity, cleanup, nil
}

type readerMultipartFile struct {
	io.Reader
}

func (f readerMultipartFile) Read(p []byte) (int, error) { return f.Reader.Read(p) }
func (readerMultipartFile) Close() error                 { return nil }
func (readerMultipartFile) ReadAt([]byte, int64) (int, error) {
	return 0, fmt.Errorf("ReadAt is unsupported")
}
func (readerMultipartFile) Seek(int64, int) (int64, error) {
	return 0, fmt.Errorf("Seek is unsupported")
}

func buildCanonicalProfessionalArchive(root string) (canonicalProfessionalArchive, error) {
	files, err := listProfessionalSkillFiles(root)
	if err != nil {
		return canonicalProfessionalArchive{}, err
	}
	files = filterRuntimeProfessionalFiles(files)
	sort.Strings(files)
	if len(files) == 0 {
		return canonicalProfessionalArchive{}, fmt.Errorf("professional skill has no runtime files")
	}
	if len(files) > maxProfessionalSkillFiles {
		return canonicalProfessionalArchive{}, fmt.Errorf("professional skill package contains too many files")
	}

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	var total int64
	written := 0
	for _, rel := range files {
		clean, err := normalizeProfessionalSkillRelativePath(rel)
		if err != nil {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, err
		}
		if isProfessionalManagementFile(clean) {
			continue
		}
		fullPath := filepath.Join(root, filepath.FromSlash(clean))
		info, err := os.Lstat(fullPath)
		if err != nil {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, fmt.Errorf(
				"symbolic links are not allowed in skill packages: %s",
				clean,
			)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > maxProfessionalSkillFileSize {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, fmt.Errorf("professional skill file too large: %s", clean)
		}
		total += info.Size()
		if total > maxProfessionalSkillTotalSize {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, fmt.Errorf(
				"professional skill package exceeds %d bytes",
				maxProfessionalSkillTotalSize,
			)
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, err
		}
		header.Name = clean
		header.Method = zip.Deflate
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		entry, err := writer.CreateHeader(header)
		if err != nil {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, err
		}
		source, err := os.Open(fullPath)
		if err != nil {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, err
		}
		_, copyErr := io.Copy(entry, source)
		closeErr := source.Close()
		if copyErr != nil {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, copyErr
		}
		if closeErr != nil {
			_ = writer.Close()
			return canonicalProfessionalArchive{}, closeErr
		}
		written++
	}
	if err := writer.Close(); err != nil {
		return canonicalProfessionalArchive{}, err
	}
	if written == 0 {
		return canonicalProfessionalArchive{}, fmt.Errorf("professional skill has no runtime files")
	}
	data := buffer.Bytes()
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	return canonicalProfessionalArchive{
		Data:      data,
		SHA256:    digest,
		FileCount: written,
	}, nil
}

func (s *Service) migrateLegacyProfessionalSkills(ctx context.Context) error {
	var records []ProfessionalSkill
	if err := s.db.WithContext(ctx).
		Where("object_path = ''").
		Where("name NOT IN ?", ReservedProfessionalSkillNames()).
		Order("created_at ASC").
		Find(&records).Error; err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	metadata, err := discoverProfessionalMetadata()
	if err != nil {
		return err
	}
	byName := make(map[string]*skills.SkillMetadata, len(metadata))
	for _, meta := range metadata {
		byName[meta.Name] = meta
	}
	migrated := 0
	for i := range records {
		record := &records[i]
		meta := byName[record.Name]
		if meta == nil {
			logger.Warnf(
				ctx,
				"[skillhub] legacy professional skill %s (%s) has no local package; it remains unavailable until re-uploaded",
				record.ID,
				record.Name,
			)
			continue
		}
		archive, err := buildCanonicalProfessionalArchive(meta.BasePath)
		if err != nil {
			return fmt.Errorf("package legacy professional skill %s: %w", record.Name, err)
		}
		objectPath, err := s.professionalStore.Reserve(
			record.TenantID,
			record.ID,
			record.ID,
		)
		if err != nil {
			return err
		}
		if err := s.professionalStore.CommitAndVerify(
			ctx,
			archive.Data,
			objectPath,
			"application/zip",
			archive.SHA256,
		); err != nil {
			return fmt.Errorf("migrate professional skill %s: %w", record.Name, err)
		}
		displayName := strings.TrimSpace(meta.DisplayName)
		if displayName == "" {
			displayName = record.Name
		}
		result := s.db.WithContext(ctx).
			Model(&ProfessionalSkill{}).
			Where("id = ? AND object_path = ''", record.ID).
			Updates(map[string]any{
				"display_name":  displayName,
				"object_path":   objectPath,
				"object_size":   int64(len(archive.Data)),
				"object_sha256": archive.SHA256,
				"file_count":    archive.FileCount,
				"updated_at":    time.Now(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			migrated++
		}
	}
	if migrated > 0 {
		logger.Infof(
			ctx,
			"[skillhub] migrated and verified %d professional skills in shared object storage",
			migrated,
		)
	}
	return nil
}

func (s *Service) deleteUnreferencedProfessionalObject(ctx context.Context, objectPath string) {
	if s == nil || s.professionalStore == nil || strings.TrimSpace(objectPath) == "" {
		return
	}
	if err := s.professionalStore.Delete(ctx, objectPath); err != nil {
		logger.Warnf(
			ctx,
			"[skillhub] failed to clean unreferenced professional skill object %s: %v",
			objectPath,
			err,
		)
	}
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}

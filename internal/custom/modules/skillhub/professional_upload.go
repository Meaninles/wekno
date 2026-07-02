package skillhub

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const (
	maxProfessionalArchiveSize       = 30 * 1024 * 1024
	maxProfessionalDescriptionLength = 1024
	maxProfessionalSkillFiles        = 300
	professionalSkillMetaFile        = ".weknora-professional-skill.json"
	professionalArchiveFile          = ".weknora-original-package"
	professionalExtractTimeout       = time.Minute
)

type ProfessionalSkillImportRequest struct {
	Name        string
	Description string
	File        multipart.File
	Filename    string
}

type ProfessionalSkillUpdateRequest struct {
	Name                string
	Description         string
	DescriptionProvided bool
	File                multipart.File
	Filename            string
}

type ProfessionalSkillDownload struct {
	Path     string
	Filename string
	Cleanup  func()
}

func (s *Service) ListProfessionalForManage(ctx context.Context) ([]ProfessionalSkillListItem, error) {
	root := getProfessionalSkillsDir()
	loader := skills.NewLoader([]string{root})
	metadata, err := loader.DiscoverSkills()
	if err != nil {
		return nil, err
	}
	access := s.professionalAccessByNameNoFail(ctx)
	if s.canManageProfessionalFromContext(ctx) {
		for _, meta := range metadata {
			if _, ok := access[meta.Name]; ok {
				continue
			}
			record, err := s.ensureProfessionalRecordForMetadata(ctx, meta)
			if err != nil {
				return nil, err
			}
			if record != nil {
				access[meta.Name] = professionalAccessEntry{
					Record:         record,
					Accessible:     true,
					IsMine:         true,
					CanManage:      true,
					SourceTenantID: record.TenantID,
					Permission:     types.OrgRoleAdmin,
				}
			}
		}
	}
	out := make([]ProfessionalSkillListItem, 0, len(metadata))
	for _, meta := range metadata {
		entry, managedRecord := access[meta.Name]
		if managedRecord && !entry.Accessible {
			continue
		}
		files, _ := loader.ListSkillFiles(meta.Name)
		files = filterRuntimeProfessionalFiles(files)
		updatedAt := professionalSkillUpdatedAt(meta.BasePath)
		item := ProfessionalSkillListItem{
			Name:        meta.Name,
			Description: "",
			Kind:        "professional",
			FileCount:   len(files),
			UpdatedAt:   updatedAt,
		}
		if managedRecord && entry.Record != nil {
			item = s.professionalItemFromAccess(entry, len(files), updatedAt)
		} else {
			item.Managed = professionalSkillManaged(meta.BasePath)
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Service) ImportProfessionalSkill(ctx context.Context, req ProfessionalSkillImportRequest) (*ProfessionalSkillListItem, error) {
	if !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleContributor) {
		return nil, fmt.Errorf("permission denied")
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	if tenantID == 0 || userID == "" {
		return nil, fmt.Errorf("tenant and user are required")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return nil, fmt.Errorf("professional skill name is required")
	}
	if err := validateProfessionalSkillName(req.Name); err != nil {
		return nil, err
	}
	if IsReservedProfessionalSkillName(req.Name) {
		return nil, fmt.Errorf("professional skill %q is system reserved", req.Name)
	}
	description, err := normalizeProfessionalDescription(req.Description)
	if err != nil {
		return nil, err
	}
	if req.File == nil {
		return nil, fmt.Errorf("professional skill package is required")
	}

	archivePath, cleanup, err := saveUploadedArchive(req.File)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	workDir, err := os.MkdirTemp("", "weknora-professional-skill-*")
	if err != nil {
		return nil, fmt.Errorf("create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	extractDir := filepath.Join(workDir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(professionalExtractTimeout)
	if err := extractProfessionalSkillArchive(archivePath, req.Filename, extractDir, deadline); err != nil {
		return nil, err
	}

	skillRoot, patchedSkill, err := validateExtractedProfessionalSkill(extractDir, req.Name)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(skillRoot, skills.SkillFileName), patchedSkill, 0o644); err != nil {
		return nil, fmt.Errorf("write normalized SKILL.md: %w", err)
	}

	root := getProfessionalSkillsDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create professional skills directory: %w", err)
	}
	target := filepath.Join(root, req.Name)
	if !pathWithin(root, target) {
		return nil, fmt.Errorf("invalid professional skill target path")
	}
	if _, err := os.Stat(target); err == nil {
		return nil, fmt.Errorf("professional skill %q already exists", req.Name)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("check professional skill target: %w", err)
	}
	staging, err := os.MkdirTemp(root, "."+req.Name+"-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	stagingMoved := false
	defer func() {
		if !stagingMoved {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := copyDir(skillRoot, staging); err != nil {
		return nil, err
	}
	if err := copyProfessionalArchive(archivePath, filepath.Join(staging, professionalArchiveFile)); err != nil {
		return nil, err
	}
	if err := writeProfessionalSkillMarker(staging, req.Filename); err != nil {
		return nil, err
	}
	if err := os.Rename(staging, target); err != nil {
		return nil, fmt.Errorf("save professional skill: %w", err)
	}
	stagingMoved = true
	record := &ProfessionalSkill{
		TenantID:        tenantID,
		CreatorID:       userID,
		Name:            req.Name,
		Description:     description,
		ArchiveFileName: cleanProfessionalArchiveFilename(req.Filename),
	}
	if err := s.db.WithContext(ctx).Create(record).Error; err != nil {
		_ = os.RemoveAll(target)
		return nil, err
	}

	item := s.professionalItemFromRecord(*record, true, "", "", countRegularFiles(target), professionalSkillUpdatedAt(target))
	return &item, nil
}

func (s *Service) UpdateProfessionalSkill(ctx context.Context, id string, req ProfessionalSkillUpdateRequest) (*ProfessionalSkillListItem, error) {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return nil, err
	}
	if IsReservedProfessionalSkillName(record.Name) {
		return nil, fmt.Errorf("professional skill %q is system reserved and cannot be modified", record.Name)
	}
	nextName := strings.TrimSpace(req.Name)
	if nextName == "" {
		return nil, fmt.Errorf("professional skill name is required")
	}
	if err := validateProfessionalSkillName(nextName); err != nil {
		return nil, err
	}
	if IsReservedProfessionalSkillName(nextName) {
		return nil, fmt.Errorf("professional skill %q is system reserved", nextName)
	}
	nextDescription := record.Description
	if req.DescriptionProvided {
		var err error
		nextDescription, err = normalizeProfessionalDescription(req.Description)
		if err != nil {
			return nil, err
		}
	}
	if nextName != record.Name {
		var count int64
		if err := s.db.WithContext(ctx).Model(&ProfessionalSkill{}).Where("name = ? AND id <> ?", nextName, record.ID).Count(&count).Error; err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, fmt.Errorf("professional skill name already exists")
		}
	}

	root, currentDir, err := professionalSkillPath(record.Name)
	if err != nil {
		return nil, err
	}
	targetDir := filepath.Join(root, nextName)
	if !pathWithin(root, targetDir) {
		return nil, fmt.Errorf("invalid professional skill target path")
	}
	if nextName != record.Name {
		if _, err := os.Stat(targetDir); err == nil {
			return nil, fmt.Errorf("professional skill %q already exists", nextName)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("check professional skill target: %w", err)
		}
	}

	staging, err := os.MkdirTemp(root, "."+nextName+"-update-*")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	stagingMoved := false
	defer func() {
		if !stagingMoved {
			_ = os.RemoveAll(staging)
		}
	}()

	nextArchiveName := record.ArchiveFileName
	if req.File != nil {
		archivePath, cleanup, err := saveUploadedArchive(req.File)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		workDir, err := os.MkdirTemp("", "weknora-professional-skill-update-*")
		if err != nil {
			return nil, fmt.Errorf("create temp directory: %w", err)
		}
		defer os.RemoveAll(workDir)
		extractDir := filepath.Join(workDir, "extract")
		if err := os.MkdirAll(extractDir, 0o755); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(professionalExtractTimeout)
		if err := extractProfessionalSkillArchive(archivePath, req.Filename, extractDir, deadline); err != nil {
			return nil, err
		}
		skillRoot, patchedSkill, err := validateExtractedProfessionalSkill(extractDir, nextName)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(skillRoot, skills.SkillFileName), patchedSkill, 0o644); err != nil {
			return nil, fmt.Errorf("write normalized SKILL.md: %w", err)
		}
		if err := copyDir(skillRoot, staging); err != nil {
			return nil, err
		}
		if err := copyProfessionalArchive(archivePath, filepath.Join(staging, professionalArchiveFile)); err != nil {
			return nil, err
		}
		nextArchiveName = cleanProfessionalArchiveFilename(req.Filename)
	} else {
		if err := copyDir(currentDir, staging); err != nil {
			return nil, err
		}
		content, err := os.ReadFile(filepath.Join(currentDir, skills.SkillFileName))
		if err != nil {
			return nil, fmt.Errorf("read existing SKILL.md: %w", err)
		}
		patchedSkill, err := normalizeSkillMarkdownName(string(content), nextName)
		if err != nil {
			return nil, err
		}
		if _, err := skills.ParseSkillFile(string(patchedSkill)); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(staging, skills.SkillFileName), patchedSkill, 0o644); err != nil {
			return nil, fmt.Errorf("write normalized SKILL.md: %w", err)
		}
	}
	if err := writeProfessionalSkillMarker(staging, nextArchiveName); err != nil {
		return nil, err
	}

	if err := replaceProfessionalSkillDir(root, currentDir, targetDir, staging); err != nil {
		return nil, err
	}
	stagingMoved = true

	if err := s.db.WithContext(ctx).Model(record).Updates(map[string]any{
		"name":              nextName,
		"description":       nextDescription,
		"archive_file_name": nextArchiveName,
		"updated_at":        time.Now(),
	}).Error; err != nil {
		return nil, err
	}
	record.Name = nextName
	record.Description = nextDescription
	record.ArchiveFileName = nextArchiveName
	updatedAt := professionalSkillUpdatedAt(targetDir)
	item := s.professionalItemFromRecord(*record, true, "", "", countRegularFiles(targetDir), updatedAt)
	return &item, nil
}

func (s *Service) DeleteProfessionalSkill(ctx context.Context, id string) error {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return err
	}
	if IsReservedProfessionalSkillName(record.Name) {
		return fmt.Errorf("professional skill %q is system reserved and cannot be deleted", record.Name)
	}
	_, target, err := professionalSkillPath(record.Name)
	if err != nil {
		return err
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
	return os.RemoveAll(target)
}

func (s *Service) DownloadProfessionalSkill(ctx context.Context, id string) (*ProfessionalSkillDownload, error) {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return nil, err
	}
	_, target, err := professionalSkillPath(record.Name)
	if err != nil {
		return nil, err
	}
	archivePath := filepath.Join(target, professionalArchiveFile)
	filename := record.ArchiveFileName
	if strings.TrimSpace(filename) == "" {
		filename = record.Name + ".zip"
	}
	if _, err := os.Stat(archivePath); err == nil {
		return &ProfessionalSkillDownload{Path: archivePath, Filename: filename}, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	download, err := createProfessionalSkillZip(record.Name, target, filename)
	if err != nil {
		return nil, err
	}
	return download, nil
}

func (s *Service) canManageProfessionalFromContext(ctx context.Context) bool {
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	return tenantID != 0 && userID != "" && types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleContributor)
}

func (s *Service) ensureProfessionalRecordForMetadata(ctx context.Context, meta *skills.SkillMetadata) (*ProfessionalSkill, error) {
	if meta == nil {
		return nil, nil
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	if tenantID == 0 || userID == "" {
		return nil, nil
	}
	var existing ProfessionalSkill
	err := s.db.WithContext(ctx).First(&existing, "name = ?", meta.Name).Error
	if err == nil {
		return &existing, nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	record := &ProfessionalSkill{
		TenantID:        tenantID,
		CreatorID:       userID,
		Name:            meta.Name,
		Description:     "",
		ArchiveFileName: meta.Name + ".zip",
	}
	if err := s.db.WithContext(ctx).Create(record).Error; err != nil {
		var raced ProfessionalSkill
		if findErr := s.db.WithContext(ctx).First(&raced, "name = ?", meta.Name).Error; findErr == nil {
			return &raced, nil
		}
		return nil, err
	}
	return record, nil
}

func createProfessionalSkillZip(skillName, dir, filename string) (*ProfessionalSkillDownload, error) {
	tmp, err := os.CreateTemp("", "weknora-professional-skill-download-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create professional skill package: %w", err)
	}
	path := tmp.Name()
	cleanup := func() { _ = os.Remove(path) }
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
			cleanup()
		}
	}()

	zw := zip.NewWriter(tmp)
	var total int64
	var count int
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if isProfessionalManagementFile(rel) {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed in skill packages: %s", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxProfessionalSkillFileSize {
			return fmt.Errorf("professional skill file too large: %s", rel)
		}
		total += info.Size()
		count++
		if count > maxProfessionalSkillFiles {
			return fmt.Errorf("professional skill package contains too many files")
		}
		if total > maxProfessionalSkillTotalSize {
			return fmt.Errorf("professional skill package exceeds %d bytes", maxProfessionalSkillTotalSize)
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = rel
		header.Method = zip.Deflate
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		_ = zw.Close()
		return nil, err
	}
	if count == 0 {
		_ = zw.Close()
		return nil, fmt.Errorf("professional skill %s has no files to download", skillName)
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	closed = true
	if strings.TrimSpace(filename) == "" {
		filename = skillName + ".zip"
	}
	return &ProfessionalSkillDownload{Path: path, Filename: filename, Cleanup: cleanup}, nil
}

func (s *Service) GetOwnedProfessionalForManage(ctx context.Context, id string) (*ProfessionalSkill, error) {
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	var record ProfessionalSkill
	if err := s.db.WithContext(ctx).First(&record, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return nil, err
	}
	if record.TenantID != tenantID {
		return nil, fmt.Errorf("permission denied")
	}
	if record.CreatorID != userID && !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin) {
		return nil, fmt.Errorf("permission denied")
	}
	return &record, nil
}

func (s *Service) ShareProfessionalToOrganization(ctx context.Context, id string, req ShareOrganizationRequest) (*OrganizationShare, error) {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return nil, err
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	member, err := s.organizationMember(ctx, req.OrganizationID, tenantID)
	if err != nil {
		return nil, err
	}
	if !member.Role.HasPermission(types.OrgRoleEditor) {
		return nil, fmt.Errorf("only space editors and admins can receive shared skills")
	}
	permission := normalizePermission(req.Permission)
	share := &OrganizationShare{
		SkillID:        record.ID,
		OrganizationID: req.OrganizationID,
		SharedByUserID: userID,
		SourceTenantID: tenantID,
		Permission:     permission,
	}
	var existing OrganizationShare
	err = s.db.WithContext(ctx).Where("skill_id = ? AND organization_id = ?", record.ID, req.OrganizationID).First(&existing).Error
	if err == nil {
		existing.Permission = permission
		existing.SharedByUserID = userID
		existing.UpdatedAt = time.Now()
		if err := s.db.WithContext(ctx).Save(&existing).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Create(share).Error; err != nil {
		return nil, err
	}
	return share, nil
}

func (s *Service) ShareProfessionalToUser(ctx context.Context, id string, req ShareUserRequest) (*UserShare, error) {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return nil, err
	}
	userID, _ := types.UserIDFromContext(ctx)
	if strings.TrimSpace(req.UserID) == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	if req.UserID == userID {
		return nil, fmt.Errorf("cannot share a skill to yourself")
	}
	permission := normalizePermission(req.Permission)
	share := &UserShare{
		SkillID:        record.ID,
		TargetUserID:   req.UserID,
		SharedByUserID: userID,
		SourceTenantID: record.TenantID,
		Permission:     permission,
	}
	var existing UserShare
	err = s.db.WithContext(ctx).Where("skill_id = ? AND target_user_id = ?", record.ID, req.UserID).First(&existing).Error
	if err == nil {
		existing.Permission = permission
		existing.SharedByUserID = userID
		existing.UpdatedAt = time.Now()
		if err := s.db.WithContext(ctx).Save(&existing).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Create(share).Error; err != nil {
		return nil, err
	}
	return share, nil
}

func (s *Service) ListProfessionalShares(ctx context.Context, id string) (*ProfessionalSkillShareList, error) {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return nil, err
	}
	var orgShares []OrganizationShare
	if err := s.db.WithContext(ctx).
		Preload("Organization").
		Preload("SharedByUser").
		Where("skill_id = ?", record.ID).
		Order("created_at DESC").
		Find(&orgShares).Error; err != nil {
		return nil, err
	}
	var userShares []UserShare
	if err := s.db.WithContext(ctx).
		Preload("TargetUser").
		Preload("SharedByUser").
		Where("skill_id = ?", record.ID).
		Order("created_at DESC").
		Find(&userShares).Error; err != nil {
		return nil, err
	}
	out := &ProfessionalSkillShareList{}
	for _, share := range orgShares {
		out.OrganizationShares = append(out.OrganizationShares, s.professionalItemFromOrgShare(*record, share, false))
	}
	for _, share := range userShares {
		out.UserShares = append(out.UserShares, s.professionalItemFromUserShare(*record, share, false))
	}
	return out, nil
}

func (s *Service) RemoveProfessionalOrganizationShare(ctx context.Context, id, shareID string) error {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Where("skill_id = ?", record.ID).Delete(&OrganizationShare{ID: shareID}).Error
}

func (s *Service) RemoveProfessionalUserShare(ctx context.Context, id, shareID string) error {
	record, err := s.GetOwnedProfessionalForManage(ctx, id)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Where("skill_id = ?", record.ID).Delete(&UserShare{ID: shareID}).Error
}

func (s *Service) professionalAccessByNameNoFail(ctx context.Context) map[string]professionalAccessEntry {
	access, err := s.professionalAccessByName(ctx)
	if err != nil {
		return map[string]professionalAccessEntry{}
	}
	return access
}

func (s *Service) professionalAccessByName(ctx context.Context) (map[string]professionalAccessEntry, error) {
	out := map[string]professionalAccessEntry{}
	if s == nil || s.db == nil {
		return out, nil
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	noAccessContext := tenantID == 0 && userID == ""
	var records []ProfessionalSkill
	if err := s.db.WithContext(ctx).Find(&records).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]*ProfessionalSkill, len(records))
	for i := range records {
		record := &records[i]
		byID[record.ID] = record
		entry := professionalAccessEntry{
			Record:         record,
			SourceTenantID: record.TenantID,
			Permission:     types.OrgRoleViewer,
		}
		if noAccessContext {
			entry.Accessible = true
		} else if tenantID != 0 && record.TenantID == tenantID {
			canManage := record.CreatorID == userID || types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin)
			entry.Accessible = true
			entry.IsMine = canManage
			entry.CanManage = canManage
			entry.Permission = types.OrgRoleAdmin
		}
		out[record.Name] = entry
	}
	if len(records) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	if tenantID != 0 {
		var orgShares []OrganizationShare
		if err := s.db.WithContext(ctx).
			Preload("Organization").
			Preload("SharedByUser").
			Joins("JOIN organization_tenant_members otm ON otm.organization_id = custom_skill_org_shares.organization_id AND otm.tenant_id = ?", tenantID).
			Where("custom_skill_org_shares.skill_id IN ?", ids).
			Order("custom_skill_org_shares.created_at DESC").
			Find(&orgShares).Error; err != nil {
			return nil, err
		}
		for _, share := range orgShares {
			record := byID[share.SkillID]
			if record == nil {
				continue
			}
			entry := out[record.Name]
			if entry.IsMine {
				continue
			}
			entry.Accessible = true
			entry.ShareID = share.ID
			entry.ShareType = shareTypeOrganization
			entry.OrganizationID = share.OrganizationID
			entry.SharedByUserID = share.SharedByUserID
			entry.SourceTenantID = share.SourceTenantID
			entry.Permission = share.Permission
			entry.SharedAt = &share.CreatedAt
			if share.Organization != nil {
				entry.OrganizationName = share.Organization.Name
			}
			if share.SharedByUser != nil {
				entry.SharedByUsername = share.SharedByUser.Username
			}
			out[record.Name] = entry
		}
	}
	if userID != "" {
		var userShares []UserShare
		if err := s.db.WithContext(ctx).
			Preload("TargetUser").
			Preload("SharedByUser").
			Where("skill_id IN ? AND target_user_id = ?", ids, userID).
			Order("created_at DESC").
			Find(&userShares).Error; err != nil {
			return nil, err
		}
		for _, share := range userShares {
			record := byID[share.SkillID]
			if record == nil {
				continue
			}
			entry := out[record.Name]
			if entry.IsMine {
				continue
			}
			entry.Accessible = true
			entry.ShareID = share.ID
			entry.ShareType = shareTypeUser
			entry.TargetUserID = share.TargetUserID
			entry.SharedByUserID = share.SharedByUserID
			entry.SourceTenantID = share.SourceTenantID
			entry.Permission = share.Permission
			entry.SharedAt = &share.CreatedAt
			if share.TargetUser != nil {
				entry.TargetUsername = share.TargetUser.Username
			}
			if share.SharedByUser != nil {
				entry.SharedByUsername = share.SharedByUser.Username
			}
			out[record.Name] = entry
		}
	}
	return out, nil
}

func (s *Service) professionalItemFromAccess(entry professionalAccessEntry, fileCount int, updatedAt *time.Time) ProfessionalSkillListItem {
	if entry.Record == nil {
		return ProfessionalSkillListItem{}
	}
	item := s.professionalItemFromRecord(*entry.Record, entry.IsMine, entry.ShareID, entry.ShareType, fileCount, updatedAt)
	item.CanManage = entry.CanManage
	item.CanDownload = entry.CanManage
	item.OrganizationID = entry.OrganizationID
	item.OrganizationName = entry.OrganizationName
	item.TargetUserID = entry.TargetUserID
	item.TargetUsername = entry.TargetUsername
	item.SharedByUserID = entry.SharedByUserID
	item.SharedByUsername = entry.SharedByUsername
	item.SourceTenantID = entry.SourceTenantID
	item.Permission = entry.Permission
	item.SharedAt = entry.SharedAt
	return item
}

func (s *Service) professionalItemFromRecord(record ProfessionalSkill, isMine bool, shareID, shareType string, fileCount int, updatedAt *time.Time) ProfessionalSkillListItem {
	systemReserved := IsReservedProfessionalSkillName(record.Name)
	return ProfessionalSkillListItem{
		ID:              record.ID,
		Name:            record.Name,
		Description:     record.Description,
		Kind:            "professional",
		FileCount:       fileCount,
		Managed:         true,
		IsMine:          isMine,
		CanManage:       isMine && !systemReserved,
		CanDownload:     isMine,
		SystemReserved:  systemReserved,
		ArchiveFileName: record.ArchiveFileName,
		ShareID:         shareID,
		ShareType:       shareType,
		SourceTenantID:  record.TenantID,
		Permission:      types.OrgRoleAdmin,
		UpdatedAt:       updatedAt,
	}
}

func (s *Service) professionalItemFromOrgShare(record ProfessionalSkill, share OrganizationShare, isMine bool) ProfessionalSkillListItem {
	item := s.professionalItemFromRecord(record, isMine, share.ID, shareTypeOrganization, 0, professionalSkillUpdatedAt(filepath.Join(getProfessionalSkillsDir(), record.Name)))
	item.OrganizationID = share.OrganizationID
	item.SharedByUserID = share.SharedByUserID
	item.SourceTenantID = share.SourceTenantID
	item.Permission = share.Permission
	item.SharedAt = &share.CreatedAt
	if share.Organization != nil {
		item.OrganizationName = share.Organization.Name
	}
	if share.SharedByUser != nil {
		item.SharedByUsername = share.SharedByUser.Username
	}
	return item
}

func (s *Service) professionalItemFromUserShare(record ProfessionalSkill, share UserShare, isMine bool) ProfessionalSkillListItem {
	item := s.professionalItemFromRecord(record, isMine, share.ID, shareTypeUser, 0, professionalSkillUpdatedAt(filepath.Join(getProfessionalSkillsDir(), record.Name)))
	item.TargetUserID = share.TargetUserID
	item.SharedByUserID = share.SharedByUserID
	item.SourceTenantID = share.SourceTenantID
	item.Permission = share.Permission
	item.SharedAt = &share.CreatedAt
	if share.TargetUser != nil {
		item.TargetUsername = share.TargetUser.Username
	}
	if share.SharedByUser != nil {
		item.SharedByUsername = share.SharedByUser.Username
	}
	return item
}

func professionalSkillPath(name string) (string, string, error) {
	root := getProfessionalSkillsDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	target := filepath.Join(root, name)
	if !pathWithin(root, target) {
		return "", "", fmt.Errorf("invalid professional skill path")
	}
	return root, target, nil
}

func copyProfessionalArchive(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func replaceProfessionalSkillDir(root, current, target, staging string) error {
	if !pathWithin(root, current) || !pathWithin(root, target) || !pathWithin(root, staging) {
		return fmt.Errorf("invalid professional skill directory")
	}
	backup := filepath.Join(root, "."+filepath.Base(current)+"-backup-"+time.Now().Format("20060102150405"))
	if _, err := os.Stat(current); err == nil {
		if err := os.Rename(current, backup); err != nil {
			return fmt.Errorf("backup existing professional skill: %w", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staging, target); err != nil {
		if _, statErr := os.Stat(backup); statErr == nil {
			_ = os.Rename(backup, current)
		}
		return fmt.Errorf("save professional skill: %w", err)
	}
	_ = os.RemoveAll(backup)
	return nil
}

func cleanProfessionalArchiveFilename(filename string) string {
	base := filepath.Base(strings.TrimSpace(filename))
	if base == "." || base == string(os.PathSeparator) || base == "" {
		return "professional-skill.zip"
	}
	return base
}

func saveUploadedArchive(file multipart.File) (string, func(), error) {
	tmp, err := os.CreateTemp("", "weknora-professional-skill-archive-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp archive: %w", err)
	}
	path := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(path)
	}
	written, err := io.Copy(tmp, io.LimitReader(file, maxProfessionalArchiveSize+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("save uploaded archive: %w", err)
	}
	if written == 0 {
		cleanup()
		return "", func() {}, fmt.Errorf("uploaded archive is empty")
	}
	if written > maxProfessionalArchiveSize {
		cleanup()
		return "", func() {}, fmt.Errorf("uploaded archive exceeds %d bytes", maxProfessionalArchiveSize)
	}
	return path, cleanup, nil
}

func extractProfessionalSkillArchive(archivePath, filename, dest string, deadline time.Time) error {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archivePath, dest, deadline)
	case strings.HasSuffix(lower, ".tar"):
		return extractTar(archivePath, dest, false, deadline)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTar(archivePath, dest, true, deadline)
	case strings.HasSuffix(lower, ".7z"), strings.HasSuffix(lower, ".rar"):
		return extractWithExternalTool(archivePath, dest, deadline)
	default:
		return fmt.Errorf("unsupported archive format; supported: .zip, .tar, .tar.gz, .tgz, .7z, .rar")
	}
}

func extractZip(archivePath, dest string, deadline time.Time) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip archive: %w", err)
	}
	defer reader.Close()

	var total int64
	for _, file := range reader.File {
		if err := checkExtractDeadline(deadline); err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if file.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed in skill packages: %s", file.Name)
		}
		rel, err := safeArchivePath(file.Name)
		if err != nil {
			return err
		}
		if file.UncompressedSize64 > maxProfessionalSkillFileSize {
			return fmt.Errorf("professional skill file too large: %s", rel)
		}
		total += int64(file.UncompressedSize64)
		if total > maxProfessionalSkillTotalSize {
			return fmt.Errorf("professional skill package exceeds %d bytes uncompressed", maxProfessionalSkillTotalSize)
		}
		src, err := file.Open()
		if err != nil {
			return fmt.Errorf("open zip file %s: %w", rel, err)
		}
		if err := writeExtractedFile(dest, rel, src, int64(file.UncompressedSize64), deadline); err != nil {
			_ = src.Close()
			return err
		}
		if err := src.Close(); err != nil {
			return err
		}
	}
	return nil
}

func extractTar(archivePath, dest string, gzipCompressed bool, deadline time.Time) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open tar archive: %w", err)
	}
	defer file.Close()

	var tarSource io.Reader = file
	if gzipCompressed {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("open gzip archive: %w", err)
		}
		defer gz.Close()
		tarSource = gz
	}
	reader := tar.NewReader(tarSource)
	var total int64
	for {
		if err := checkExtractDeadline(deadline); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar archive: %w", err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return fmt.Errorf("unsupported archive entry type for %s", header.Name)
		}
		rel, err := safeArchivePath(header.Name)
		if err != nil {
			return err
		}
		if header.Size > maxProfessionalSkillFileSize {
			return fmt.Errorf("professional skill file too large: %s", rel)
		}
		total += header.Size
		if total > maxProfessionalSkillTotalSize {
			return fmt.Errorf("professional skill package exceeds %d bytes uncompressed", maxProfessionalSkillTotalSize)
		}
		if err := writeExtractedFile(dest, rel, reader, header.Size, deadline); err != nil {
			return err
		}
	}
	return nil
}

func extractWithExternalTool(archivePath, dest string, deadline time.Time) error {
	type candidate struct {
		name string
		args []string
	}
	candidates := []candidate{
		{name: "7zz", args: []string{"x", "-y", "-o" + dest, archivePath}},
		{name: "7z", args: []string{"x", "-y", "-o" + dest, archivePath}},
		{name: "7za", args: []string{"x", "-y", "-o" + dest, archivePath}},
		{name: "bsdtar", args: []string{"-xf", archivePath, "-C", dest}},
		{name: "unar", args: []string{"-force-overwrite", "-o", dest, archivePath}},
		{name: "unrar", args: []string{"x", "-o+", archivePath, dest + string(os.PathSeparator)}},
	}
	var attempted []string
	var lastErr error
	for _, item := range candidates {
		if _, err := exec.LookPath(item.name); err != nil {
			continue
		}
		attempted = append(attempted, item.name)
		if err := clearDirectory(dest); err != nil {
			return fmt.Errorf("prepare extraction directory: %w", err)
		}
		timeout := time.Until(deadline)
		if timeout <= 0 {
			return fmt.Errorf("professional skill archive extraction exceeded %s", professionalExtractTimeout)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cmd := exec.CommandContext(ctx, item.name, item.args...)
		output, err := cmd.CombinedOutput()
		cancel()
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("professional skill archive extraction exceeded %s", professionalExtractTimeout)
		}
		if err != nil {
			lastErr = fmt.Errorf("%s: %s", item.name, strings.TrimSpace(string(output)))
			continue
		}
		if err := validateExtractedTree(dest); err != nil {
			lastErr = fmt.Errorf("%s: %w", item.name, err)
			continue
		}
		return nil
	}
	if len(attempted) > 0 && lastErr != nil {
		return fmt.Errorf("extract archive failed with available tools (%s): %w", strings.Join(attempted, ", "), lastErr)
	}
	return fmt.Errorf("unsupported archive format on this server; install 7z, bsdtar, unar, or unrar to import .7z/.rar packages")
}

func clearDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func checkExtractDeadline(deadline time.Time) error {
	if !deadline.IsZero() && time.Now().After(deadline) {
		return fmt.Errorf("professional skill archive extraction exceeded %s", professionalExtractTimeout)
	}
	return nil
}

func writeExtractedFile(root, rel string, src io.Reader, expectedSize int64, deadline time.Time) error {
	if expectedSize > maxProfessionalSkillFileSize {
		return fmt.Errorf("professional skill file too large: %s", rel)
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	if !pathWithin(root, target) {
		return fmt.Errorf("archive entry escapes extraction directory: %s", rel)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create extracted file %s: %w", rel, err)
	}
	_, copyErr := io.Copy(dst, io.LimitReader(deadlineReader{reader: src, deadline: deadline}, maxProfessionalSkillFileSize+1))
	closeErr := dst.Close()
	if copyErr != nil {
		return fmt.Errorf("write extracted file %s: %w", rel, copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if info.Size() > maxProfessionalSkillFileSize {
		return fmt.Errorf("professional skill file too large: %s", rel)
	}
	return nil
}

type deadlineReader struct {
	reader   io.Reader
	deadline time.Time
}

func (r deadlineReader) Read(p []byte) (int, error) {
	if err := checkExtractDeadline(r.deadline); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err != nil {
		return n, err
	}
	if deadlineErr := checkExtractDeadline(r.deadline); deadlineErr != nil {
		return n, deadlineErr
	}
	return n, nil
}

func safeArchivePath(raw string) (string, error) {
	rel := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid archive entry path: %s", raw)
	}
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("invalid archive entry path: %s", raw)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return "", fmt.Errorf("invalid archive entry path: %s", raw)
		}
	}
	return clean, nil
}

func validateProfessionalSkillName(name string) error {
	if len(name) > skills.MaxNameLength {
		return fmt.Errorf("professional skill name exceeds %d characters", skills.MaxNameLength)
	}
	for i, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !valid {
			return fmt.Errorf("professional skill name must use lowercase letters, numbers, and hyphens only")
		}
		if i == 0 && r == '-' {
			return fmt.Errorf("professional skill name must not start with a hyphen")
		}
	}
	if strings.HasSuffix(name, "-") {
		return fmt.Errorf("professional skill name must not end with a hyphen")
	}
	if strings.Contains(name, "--") {
		return fmt.Errorf("professional skill name must not contain consecutive hyphens")
	}
	return nil
}

func normalizeProfessionalDescription(raw string) (string, error) {
	description := strings.TrimSpace(raw)
	if len([]rune(description)) > maxProfessionalDescriptionLength {
		return "", fmt.Errorf("professional skill description exceeds %d characters", maxProfessionalDescriptionLength)
	}
	return description, nil
}

func validateExtractedTree(root string) error {
	var total int64
	var count int
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed in skill packages")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxProfessionalSkillFileSize {
			return fmt.Errorf("professional skill file too large: %s", path)
		}
		total += info.Size()
		count++
		if count > maxProfessionalSkillFiles {
			return fmt.Errorf("professional skill package contains too many files")
		}
		if total > maxProfessionalSkillTotalSize {
			return fmt.Errorf("professional skill package exceeds %d bytes uncompressed", maxProfessionalSkillTotalSize)
		}
		return nil
	})
}

func validateExtractedProfessionalSkill(root, requestedName string) (string, []byte, error) {
	if err := validateExtractedTree(root); err != nil {
		return "", nil, err
	}
	var skillFiles []string
	var regularFiles []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if filepath.Base(path) == skills.SkillFileName {
			skillFiles = append(skillFiles, path)
		}
		if !isArchiveJunk(rel) {
			regularFiles = append(regularFiles, path)
		}
		return nil
	}); err != nil {
		return "", nil, err
	}
	if len(skillFiles) != 1 {
		return "", nil, fmt.Errorf("professional skill package must contain exactly one SKILL.md")
	}
	skillRoot := filepath.Dir(skillFiles[0])
	for _, path := range regularFiles {
		if !pathWithin(skillRoot, path) {
			return "", nil, fmt.Errorf("professional skill package must contain one skill directory; file outside skill root: %s", path)
		}
	}

	content, err := os.ReadFile(skillFiles[0])
	if err != nil {
		return "", nil, fmt.Errorf("read SKILL.md: %w", err)
	}
	parsed, err := skills.ParseSkillFile(string(content))
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(parsed.Instructions) == "" {
		return "", nil, fmt.Errorf("SKILL.md body is required")
	}
	patched, err := normalizeSkillMarkdownName(string(content), requestedName)
	if err != nil {
		return "", nil, err
	}
	if _, err := skills.ParseSkillFile(string(patched)); err != nil {
		return "", nil, err
	}
	return skillRoot, patched, nil
}

func normalizeSkillMarkdownName(content, name string) ([]byte, error) {
	frontmatter, body, err := skillMarkdownParts(content)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(frontmatter, "\n")
	replaced := false
	for i, line := range lines {
		if strings.TrimLeft(line, " \t") != line {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, "name:") {
			continue
		}
		lines[i] = "name: " + name
		replaced = true
		break
	}
	if !replaced {
		return nil, fmt.Errorf("SKILL.md frontmatter must include top-level name")
	}
	return []byte("---\n" + strings.Join(lines, "\n") + "\n---\n\n" + strings.TrimSpace(body) + "\n"), nil
}

func skillMarkdownParts(content string) (string, string, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", fmt.Errorf("SKILL.md must start with YAML frontmatter")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), nil
		}
	}
	return "", "", fmt.Errorf("SKILL.md frontmatter is not properly closed with ---")
}

func isArchiveJunk(rel string) bool {
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	return strings.HasPrefix(rel, "__MACOSX/") || base == ".DS_Store" || base == "Thumbs.db"
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed in skill packages")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()
		dstFile, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(dstFile, srcFile)
		closeErr := dstFile.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func pathWithin(root, path string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")
}

func writeProfessionalSkillMarker(dir, archiveFilename string) error {
	data, err := json.Marshal(map[string]any{
		"managed_by":        "weknora",
		"created_at":        time.Now().UTC().Format(time.RFC3339),
		"archive_file_name": cleanProfessionalArchiveFilename(archiveFilename),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, professionalSkillMetaFile), data, 0o644)
}

func professionalSkillManaged(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, professionalSkillMetaFile))
	return err == nil
}

func professionalSkillUpdatedAt(dir string) *time.Time {
	if dir == "" {
		return nil
	}
	var latest time.Time
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	if latest.IsZero() {
		return nil
	}
	return &latest
}

func countRegularFiles(dir string) int {
	count := 0
	_ = filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && !isProfessionalManagementFile(entry.Name()) {
			count++
		}
		return nil
	})
	return count
}

func filterRuntimeProfessionalFiles(files []string) []string {
	out := files[:0]
	for _, file := range files {
		if isProfessionalManagementFile(filepath.ToSlash(file)) {
			continue
		}
		out = append(out, file)
	}
	return out
}

func isProfessionalManagementFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	return rel == professionalSkillMetaFile || rel == professionalArchiveFile
}

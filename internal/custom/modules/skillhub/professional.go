package skillhub

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	DefaultProfessionalSkillsDir  = "skills/professional"
	maxProfessionalSkillFileSize  = 10 * 1024 * 1024
	maxProfessionalSkillTotalSize = 50 * 1024 * 1024
)

var reservedProfessionalSkillNames = []string{
	"anysearch-skill",
	"find-skill-skillhub",
}

func ReservedProfessionalSkillNames() []string {
	out := make([]string, len(reservedProfessionalSkillNames))
	copy(out, reservedProfessionalSkillNames)
	return out
}

func IsReservedProfessionalSkillName(name string) bool {
	name = strings.TrimSpace(name)
	for _, reserved := range reservedProfessionalSkillNames {
		if name == reserved {
			return true
		}
	}
	return false
}

type ProfessionalSkillPackage struct {
	Name        string                  `json:"name"`
	DisplayName string                  `json:"display_name,omitempty"`
	Description string                  `json:"description"`
	Files       []ProfessionalSkillFile `json:"files"`
}

type ProfessionalSkillFile struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
}

type professionalAccessEntry struct {
	Record           *ProfessionalSkill
	Accessible       bool
	IsMine           bool
	CanManage        bool
	ShareID          string
	ShareType        string
	OrganizationID   string
	OrganizationName string
	TargetUserID     string
	TargetUsername   string
	SharedByUserID   string
	SharedByUsername string
	SourceTenantID   uint64
	Permission       types.OrgMemberRole
	SharedAt         *time.Time
}

// ProfessionalMetadata remains as the filesystem seam for immutable skills
// shipped in the application image. Uploaded skills are intentionally absent;
// callers that need tenant-aware metadata must use Service.ProfessionalMetadata.
func ProfessionalMetadata(_ context.Context) ([]*skills.SkillMetadata, error) {
	metadata, err := discoverReservedProfessionalMetadata()
	if err != nil {
		return nil, err
	}
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Name < metadata[j].Name })
	return metadata, nil
}

// ProfessionalPackages packages only immutable image skills. Tenant-owned
// uploaded skills are loaded through Service.ProfessionalPackages.
func ProfessionalPackages(
	_ context.Context,
	names []string,
	all bool,
) ([]ProfessionalSkillPackage, error) {
	metadata, err := discoverReservedProfessionalMetadata()
	if err != nil {
		return nil, err
	}
	return packageFilesystemProfessionalSkills(metadata, names, all)
}

func (s *Service) ProfessionalMetadata(ctx context.Context) ([]*skills.SkillMetadata, error) {
	metadata, err := discoverReservedProfessionalMetadata()
	if err != nil {
		return nil, err
	}
	access, err := s.professionalAccessByName(ctx)
	if err != nil {
		return nil, err
	}
	for _, entry := range access {
		if !entry.Accessible || entry.Record == nil {
			continue
		}
		record := entry.Record
		displayName := strings.TrimSpace(record.DisplayName)
		if displayName == "" {
			displayName = record.Name
		}
		metadata = append(metadata, &skills.SkillMetadata{
			Name:        record.Name,
			DisplayName: displayName,
			Description: record.Description,
		})
	}
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Name < metadata[j].Name })
	return metadata, nil
}

func (s *Service) ProfessionalPackages(
	ctx context.Context,
	names []string,
	all bool,
) ([]ProfessionalSkillPackage, error) {
	requested := make(map[string]bool)
	for _, name := range normalizeNames(names) {
		requested[name] = true
	}

	reservedMetadata, err := discoverReservedProfessionalMetadata()
	if err != nil {
		return nil, err
	}
	reservedPackages, err := packageFilesystemProfessionalSkills(reservedMetadata, names, all)
	if err != nil {
		return nil, err
	}

	access, err := s.professionalAccessByName(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]ProfessionalSkill, 0, len(access))
	for _, entry := range access {
		if !entry.Accessible || entry.Record == nil {
			continue
		}
		if !all && !requested[entry.Record.Name] {
			continue
		}
		records = append(records, *entry.Record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })

	packages := make([]ProfessionalSkillPackage, 0, len(reservedPackages)+len(records))
	packages = append(packages, reservedPackages...)
	var total int64
	for _, pkg := range reservedPackages {
		for _, file := range pkg.Files {
			content, decodeErr := base64.StdEncoding.DecodeString(file.ContentBase64)
			if decodeErr != nil {
				return nil, decodeErr
			}
			total += int64(len(content))
		}
	}
	for i := range records {
		record := &records[i]
		archive, readErr := s.readProfessionalObject(ctx, record)
		if readErr != nil {
			return nil, fmt.Errorf("load professional skill %s: %w", record.Name, readErr)
		}
		pkg, decodeErr := decodeProfessionalSkillArchive(record, archive, &total)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode professional skill %s: %w", record.Name, decodeErr)
		}
		packages = append(packages, pkg)
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	return packages, nil
}

func (s *Service) readProfessionalObject(
	ctx context.Context,
	record *ProfessionalSkill,
) ([]byte, error) {
	if record == nil || strings.TrimSpace(record.ObjectPath) == "" {
		return nil, fmt.Errorf("professional skill object is unavailable")
	}
	if s == nil || s.professionalStore == nil {
		if s != nil && s.professionalStoreErr != nil {
			return nil, s.professionalStoreErr
		}
		return nil, fmt.Errorf("professional skill object storage is unavailable")
	}
	if record.ObjectSize <= 0 || record.ObjectSize > maxProfessionalSkillTotalSize {
		return nil, fmt.Errorf("invalid professional skill object size %d", record.ObjectSize)
	}
	if err := s.professionalStore.Verify(
		ctx,
		record.ObjectPath,
		record.ObjectSize,
		record.ObjectSHA256,
	); err != nil {
		return nil, fmt.Errorf("verify object: %w", err)
	}
	reader, err := s.professionalStore.Open(ctx, record.ObjectPath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, record.ObjectSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != record.ObjectSize {
		return nil, fmt.Errorf(
			"professional skill object size mismatch: got %d, want %d",
			len(content),
			record.ObjectSize,
		)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	if digest != record.ObjectSHA256 {
		return nil, fmt.Errorf("professional skill object digest mismatch")
	}
	return content, nil
}

func decodeProfessionalSkillArchive(
	record *ProfessionalSkill,
	archive []byte,
	total *int64,
) (ProfessionalSkillPackage, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return ProfessionalSkillPackage{}, err
	}
	displayName := strings.TrimSpace(record.DisplayName)
	if displayName == "" {
		displayName = record.Name
	}
	pkg := ProfessionalSkillPackage{
		Name:        record.Name,
		DisplayName: displayName,
		Description: record.Description,
		Files:       make([]ProfessionalSkillFile, 0, len(zr.File)),
	}
	var packageTotal int64
	for _, file := range zr.File {
		if file.FileInfo().IsDir() {
			continue
		}
		clean, err := normalizeProfessionalSkillRelativePath(file.Name)
		if err != nil {
			return ProfessionalSkillPackage{}, err
		}
		if isProfessionalManagementFile(clean) {
			continue
		}
		if file.UncompressedSize64 > maxProfessionalSkillFileSize {
			return ProfessionalSkillPackage{}, fmt.Errorf("file too large: %s", clean)
		}
		packageTotal += int64(file.UncompressedSize64)
		if packageTotal > maxProfessionalSkillTotalSize {
			return ProfessionalSkillPackage{}, fmt.Errorf("package exceeds %d bytes", maxProfessionalSkillTotalSize)
		}
		*total += int64(file.UncompressedSize64)
		if *total > maxProfessionalSkillTotalSize {
			return ProfessionalSkillPackage{}, fmt.Errorf(
				"professional skills payload exceeds %d bytes",
				maxProfessionalSkillTotalSize,
			)
		}
		reader, err := file.Open()
		if err != nil {
			return ProfessionalSkillPackage{}, err
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, maxProfessionalSkillFileSize+1))
		closeErr := reader.Close()
		if readErr != nil {
			return ProfessionalSkillPackage{}, readErr
		}
		if closeErr != nil {
			return ProfessionalSkillPackage{}, closeErr
		}
		if len(content) > maxProfessionalSkillFileSize {
			return ProfessionalSkillPackage{}, fmt.Errorf("file too large: %s", clean)
		}
		if clean == skills.SkillFileName {
			content, err = normalizeSkillMarkdownForRuntime(
				string(content),
				record.Name,
				displayName,
			)
			if err != nil {
				return ProfessionalSkillPackage{}, err
			}
		}
		pkg.Files = append(pkg.Files, ProfessionalSkillFile{
			Path:          clean,
			ContentBase64: base64.StdEncoding.EncodeToString(content),
		})
	}
	if len(pkg.Files) == 0 {
		return ProfessionalSkillPackage{}, fmt.Errorf("professional skill has no files")
	}
	sort.Slice(pkg.Files, func(i, j int) bool { return pkg.Files[i].Path < pkg.Files[j].Path })
	return pkg, nil
}

func packageFilesystemProfessionalSkills(
	metadata []*skills.SkillMetadata,
	names []string,
	all bool,
) ([]ProfessionalSkillPackage, error) {
	selected := metadata
	if !all {
		allowed := make(map[string]bool, len(names))
		for _, name := range normalizeNames(names) {
			allowed[name] = true
		}
		selected = make([]*skills.SkillMetadata, 0, len(metadata))
		for _, meta := range metadata {
			if allowed[meta.Name] {
				selected = append(selected, meta)
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })

	var total int64
	packages := make([]ProfessionalSkillPackage, 0, len(selected))
	for _, meta := range selected {
		files, err := listProfessionalSkillFiles(meta.BasePath)
		if err != nil {
			return nil, fmt.Errorf("list professional skill %s files: %w", meta.Name, err)
		}
		sort.Strings(files)
		pkg := ProfessionalSkillPackage{
			Name:        meta.Name,
			DisplayName: meta.DisplayName,
			Description: meta.Description,
			Files:       make([]ProfessionalSkillFile, 0, len(files)),
		}
		for _, rel := range files {
			clean, err := normalizeProfessionalSkillRelativePath(rel)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid professional skill file path %s/%s: %w",
					meta.Name,
					rel,
					err,
				)
			}
			if isProfessionalManagementFile(clean) {
				continue
			}
			fullPath := filepath.Join(meta.BasePath, filepath.FromSlash(clean))
			info, err := os.Lstat(fullPath)
			if err != nil {
				return nil, fmt.Errorf("stat professional skill file %s/%s: %w", meta.Name, rel, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf(
					"symbolic links are not allowed in skill packages: %s/%s",
					meta.Name,
					clean,
				)
			}
			if info.IsDir() {
				continue
			}
			if info.Size() > maxProfessionalSkillFileSize {
				return nil, fmt.Errorf("professional skill file too large: %s/%s", meta.Name, rel)
			}
			total += info.Size()
			if total > maxProfessionalSkillTotalSize {
				return nil, fmt.Errorf(
					"professional skills payload exceeds %d bytes",
					maxProfessionalSkillTotalSize,
				)
			}
			content, err := os.ReadFile(fullPath)
			if err != nil {
				return nil, fmt.Errorf("read professional skill file %s/%s: %w", meta.Name, rel, err)
			}
			if clean == skills.SkillFileName {
				content, err = normalizeSkillMarkdownForRuntime(
					string(content),
					meta.Name,
					meta.DisplayName,
				)
				if err != nil {
					return nil, fmt.Errorf("normalize runtime professional skill %s: %w", meta.Name, err)
				}
			}
			pkg.Files = append(pkg.Files, ProfessionalSkillFile{
				Path:          clean,
				ContentBase64: base64.StdEncoding.EncodeToString(content),
			})
		}
		if len(pkg.Files) == 0 {
			return nil, fmt.Errorf("professional skill %s has no files", meta.Name)
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

func discoverReservedProfessionalMetadata() ([]*skills.SkillMetadata, error) {
	metadata, err := discoverProfessionalMetadata()
	if err != nil {
		return nil, err
	}
	out := make([]*skills.SkillMetadata, 0, len(reservedProfessionalSkillNames))
	for _, meta := range metadata {
		if IsReservedProfessionalSkillName(meta.Name) {
			out = append(out, meta)
		}
	}
	return out, nil
}

func discoverProfessionalMetadata() ([]*skills.SkillMetadata, error) {
	root := getProfessionalSkillsDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	metadata := make([]*skills.SkillMetadata, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		basePath := filepath.Join(root, entry.Name())
		meta, err := readProfessionalMetadataFromDir(basePath)
		if err != nil {
			continue
		}
		metadata = append(metadata, meta)
	}
	return metadata, nil
}

func readProfessionalMetadataFromDir(basePath string) (*skills.SkillMetadata, error) {
	content, err := os.ReadFile(filepath.Join(basePath, skills.SkillFileName))
	if err != nil {
		return nil, err
	}
	marker := readProfessionalSkillMarker(basePath)
	name := strings.TrimSpace(marker.RuntimeName)
	displayName := strings.TrimSpace(marker.DisplayName)
	identity, identityErr := resolveProfessionalSkillIdentity(
		string(content),
		name,
		displayName,
		filepath.Base(basePath),
		marker.ArchiveFileName,
	)
	skill, parseErr := skills.ParseSkillFile(string(content))
	if identityErr != nil && parseErr != nil {
		return nil, parseErr
	}
	if name == "" || !isValidProfessionalSkillName(name) {
		if identityErr == nil {
			name = identity.RuntimeName
		} else {
			name = skill.Name
		}
	}
	description := ""
	if identityErr == nil {
		if displayName == "" {
			displayName = identity.DisplayName
		}
		description = identity.Description
	} else {
		if displayName == "" {
			displayName = strings.TrimSpace(skill.DisplayName)
		}
		description = skill.Description
	}
	if displayName == "" {
		displayName = name
	}
	return &skills.SkillMetadata{
		Name:        name,
		DisplayName: displayName,
		Description: description,
		BasePath:    basePath,
	}, nil
}

func listProfessionalSkillFiles(basePath string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(basePath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(basePath, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}

func getProfessionalSkillsDir() string {
	if dir := strings.TrimSpace(os.Getenv("WEKNORA_PROFESSIONAL_SKILLS_DIR")); dir != "" {
		return dir
	}
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(execPath), DefaultProfessionalSkillsDir)
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		dir := filepath.Join(cwd, DefaultProfessionalSkillsDir)
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	return DefaultProfessionalSkillsDir
}

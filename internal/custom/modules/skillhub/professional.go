package skillhub

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

var professionalAccess = struct {
	sync.RWMutex
	resolver func(context.Context) (map[string]professionalAccessEntry, error)
}{}

func registerProfessionalAccessResolver(fn func(context.Context) (map[string]professionalAccessEntry, error)) {
	professionalAccess.Lock()
	defer professionalAccess.Unlock()
	professionalAccess.resolver = fn
}

func ProfessionalMetadata(ctx context.Context) ([]*skills.SkillMetadata, error) {
	metadata, err := discoverProfessionalMetadata()
	if err != nil {
		return nil, err
	}
	metadata = filterAccessibleProfessionalMetadata(ctx, metadata)
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Name < metadata[j].Name })
	return metadata, nil
}

func ProfessionalPackages(ctx context.Context, names []string, all bool) ([]ProfessionalSkillPackage, error) {
	_ = ctx
	metadata, err := discoverProfessionalMetadata()
	if err != nil {
		return nil, err
	}
	if len(metadata) == 0 {
		return nil, nil
	}
	metadata = filterAccessibleProfessionalMetadata(ctx, metadata)
	selected := metadata
	if !all {
		allowed := make(map[string]bool, len(names))
		for _, name := range normalizeNames(names) {
			allowed[name] = true
		}
		selected = selected[:0]
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
				return nil, fmt.Errorf("invalid professional skill file path %s/%s: %w", meta.Name, rel, err)
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
				return nil, fmt.Errorf("symbolic links are not allowed in skill packages: %s/%s", meta.Name, clean)
			}
			if info.IsDir() {
				continue
			}
			if info.Size() > maxProfessionalSkillFileSize {
				return nil, fmt.Errorf("professional skill file too large: %s/%s", meta.Name, rel)
			}
			total += info.Size()
			if total > maxProfessionalSkillTotalSize {
				return nil, fmt.Errorf("professional skills payload exceeds %d bytes", maxProfessionalSkillTotalSize)
			}
			content, err := os.ReadFile(fullPath)
			if err != nil {
				return nil, fmt.Errorf("read professional skill file %s/%s: %w", meta.Name, rel, err)
			}
			if clean == skills.SkillFileName {
				content, err = normalizeSkillMarkdownForRuntime(string(content), meta.Name, meta.DisplayName)
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
	identity, identityErr := resolveProfessionalSkillIdentity(string(content), name, displayName, filepath.Base(basePath), marker.ArchiveFileName)
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

func resolveProfessionalAccess(ctx context.Context) map[string]professionalAccessEntry {
	professionalAccess.RLock()
	resolver := professionalAccess.resolver
	professionalAccess.RUnlock()
	if resolver == nil {
		return nil
	}
	access, err := resolver(ctx)
	if err != nil {
		return nil
	}
	return access
}

func filterAccessibleProfessionalMetadata(ctx context.Context, metadata []*skills.SkillMetadata) []*skills.SkillMetadata {
	access := resolveProfessionalAccess(ctx)
	if access == nil {
		return metadata
	}
	out := metadata[:0]
	for _, meta := range metadata {
		if IsReservedProfessionalSkillName(meta.Name) {
			out = append(out, meta)
			continue
		}
		entry, managed := access[meta.Name]
		if managed && !entry.Accessible {
			continue
		}
		out = append(out, meta)
	}
	return out
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

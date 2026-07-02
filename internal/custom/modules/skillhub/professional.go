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

func IsReservedProfessionalSkillName(name string) bool {
	return false
}

type ProfessionalSkillPackage struct {
	Name        string                  `json:"name"`
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
	loader := skills.NewLoader([]string{getProfessionalSkillsDir()})
	metadata, err := loader.DiscoverSkills()
	if err != nil {
		return nil, err
	}
	metadata = filterAccessibleProfessionalMetadata(ctx, metadata)
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Name < metadata[j].Name })
	return metadata, nil
}

func ProfessionalPackages(ctx context.Context, names []string, all bool) ([]ProfessionalSkillPackage, error) {
	_ = ctx
	loader := skills.NewLoader([]string{getProfessionalSkillsDir()})
	metadata, err := loader.DiscoverSkills()
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
		skill, err := loader.LoadSkillInstructions(meta.Name)
		if err != nil {
			return nil, fmt.Errorf("load professional skill %s: %w", meta.Name, err)
		}
		files, err := loader.ListSkillFiles(meta.Name)
		if err != nil {
			return nil, fmt.Errorf("list professional skill %s files: %w", meta.Name, err)
		}
		sort.Strings(files)
		pkg := ProfessionalSkillPackage{
			Name:        skill.Name,
			Description: skill.Description,
			Files:       make([]ProfessionalSkillFile, 0, len(files)),
		}
		for _, rel := range files {
			if isProfessionalManagementFile(filepath.ToSlash(rel)) {
				continue
			}
			clean := filepath.Clean(rel)
			if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
				return nil, fmt.Errorf("invalid professional skill file path %s/%s", skill.Name, rel)
			}
			fullPath := filepath.Join(skill.BasePath, clean)
			info, err := os.Stat(fullPath)
			if err != nil {
				return nil, fmt.Errorf("stat professional skill file %s/%s: %w", skill.Name, rel, err)
			}
			if info.IsDir() {
				continue
			}
			if info.Size() > maxProfessionalSkillFileSize {
				return nil, fmt.Errorf("professional skill file too large: %s/%s", skill.Name, rel)
			}
			total += info.Size()
			if total > maxProfessionalSkillTotalSize {
				return nil, fmt.Errorf("professional skills payload exceeds %d bytes", maxProfessionalSkillTotalSize)
			}
			content, err := os.ReadFile(fullPath)
			if err != nil {
				return nil, fmt.Errorf("read professional skill file %s/%s: %w", skill.Name, rel, err)
			}
			pkg.Files = append(pkg.Files, ProfessionalSkillFile{
				Path:          filepath.ToSlash(clean),
				ContentBase64: base64.StdEncoding.EncodeToString(content),
			})
		}
		if len(pkg.Files) == 0 {
			return nil, fmt.Errorf("professional skill %s has no files", skill.Name)
		}
		packages = append(packages, pkg)
	}
	return packages, nil
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

package skillhub

import (
	"context"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/skills"
)

const (
	LightweightSkillSourceManaged   = "managed"
	LightweightSkillSourcePreloaded = "preloaded"
	LightweightSkillDropUnavailable = "unavailable"
)

// LightweightSkillPackage is the permission-checked, model-ready form of a
// lightweight prompt skill. Key is stable for a managed skill and namespaced
// for an immutable preloaded skill so model-facing identity never depends on a
// display label alone.
type LightweightSkillPackage struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Source       string `json:"source"`
}

type LightweightSkillDrop struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// LightweightPackages resolves the one authoritative lightweight-skill set
// for a run. Agent-configured defaults and chat selections are unioned, then
// resolved through the same permission-aware lookup and deduplicated by name.
// In all mode, tenant-accessible managed skills override same-named preloaded
// skills, matching selected-mode behavior.
func (s *Service) LightweightPackages(
	ctx context.Context,
	mode string,
	configuredNames []string,
	chatNames []string,
) ([]LightweightSkillPackage, []LightweightSkillDrop, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "none"
	}
	if mode != "all" {
		requestedNames := append([]string(nil), chatNames...)
		if mode == "selected" {
			requestedNames = append(append([]string(nil), configuredNames...), chatNames...)
		}
		if len(normalizeNames(requestedNames)) == 0 {
			return nil, nil, nil
		}
	}

	accessible, err := s.accessibleByName(ctx)
	if err != nil {
		return nil, nil, err
	}
	preloaded := skills.NewLoader([]string{getPreloadedSkillsDir()})

	names := make([]string, 0, len(configuredNames)+len(chatNames))
	if mode == "all" {
		for name := range accessible {
			names = append(names, name)
		}
		if metadata, discoverErr := preloaded.DiscoverSkills(); discoverErr == nil {
			for _, meta := range metadata {
				if meta == nil {
					continue
				}
				name := strings.TrimSpace(meta.Name)
				if name == "" {
					continue
				}
				if _, managed := accessible[name]; managed {
					continue
				}
				names = append(names, name)
			}
		}
		sort.Strings(names)
	} else if mode == "selected" {
		names = append(names, configuredNames...)
	}
	// Chat-selected lightweight skills are turn-scoped additions. Appending
	// before normalization makes selecting an already-effective skill a no-op.
	names = append(names, chatNames...)
	names = normalizeNames(names)

	packages := make([]LightweightSkillPackage, 0, len(names))
	dropped := make([]LightweightSkillDrop, 0)
	for _, name := range names {
		if item, ok := accessible[name]; ok {
			packages = append(packages, LightweightSkillPackage{
				Key:          "lightweight:managed:" + item.ID,
				Name:         strings.TrimSpace(item.Name),
				Description:  strings.TrimSpace(item.Description),
				Instructions: strings.TrimSpace(item.Instructions),
				Source:       LightweightSkillSourceManaged,
			})
			continue
		}
		skill, loadErr := preloaded.LoadSkillInstructions(name)
		if loadErr != nil || skill == nil {
			dropped = append(dropped, LightweightSkillDrop{Name: name, Reason: LightweightSkillDropUnavailable})
			continue
		}
		resolvedName := strings.TrimSpace(skill.Name)
		if resolvedName == "" {
			resolvedName = name
		}
		packages = append(packages, LightweightSkillPackage{
			Key:          "lightweight:preloaded:" + resolvedName,
			Name:         resolvedName,
			Description:  strings.TrimSpace(skill.Description),
			Instructions: strings.TrimSpace(skill.Instructions),
			Source:       LightweightSkillSourcePreloaded,
		})
	}
	return packages, dropped, nil
}

func renderLightweightPackages(title string, packages []LightweightSkillPackage) string {
	if len(packages) == 0 {
		return ""
	}
	sections := make([]string, 0, len(packages))
	for _, item := range packages {
		sections = append(sections, renderContextSection(item.Name, item.Description, item.Instructions))
	}
	return title + "\n" + strings.Join(sections, "\n\n")
}

// EffectiveLightweightSkillContext is the native-prompt adapter for the same
// resolver used by SDK-based agents. Keeping mode handling here prevents
// AgentQA, KnowledgeQA, and the sidecar path from developing separate
// configured-vs-chat selection semantics.
func (s *Service) EffectiveLightweightSkillContext(
	ctx context.Context,
	mode string,
	configuredNames []string,
	chatNames []string,
) (string, error) {
	packages, dropped, err := s.LightweightPackages(ctx, mode, configuredNames, chatNames)
	if err != nil {
		return "", err
	}
	if len(dropped) > 0 {
		s.DebugLog(ctx, "dropped unavailable lightweight skills: %v", dropped)
	}
	return renderLightweightPackages("[本轮有效轻量 Skills]", packages), nil
}

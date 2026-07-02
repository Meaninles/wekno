package service

import (
	"context"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

type AdditionalSkillLister func(context.Context) ([]*skills.SkillMetadata, error)
type ProfessionalSkillLister func(context.Context) ([]*skills.SkillMetadata, error)
type RuntimeSkillConfigurer func(context.Context, *types.QARequest, *types.AgentConfig, *types.CustomAgent) error
type SelectedSkillContextResolver func(context.Context, []string) (string, error)
type AllSkillContextResolver func(context.Context) (string, error)

var skillHookRegistry = struct {
	sync.RWMutex
	listers             []AdditionalSkillLister
	professionalListers []ProfessionalSkillLister
	configurers         []RuntimeSkillConfigurer
	resolvers           []SelectedSkillContextResolver
	allResolvers        []AllSkillContextResolver
}{}

func RegisterAdditionalSkillLister(fn AdditionalSkillLister) {
	if fn == nil {
		return
	}
	skillHookRegistry.Lock()
	defer skillHookRegistry.Unlock()
	skillHookRegistry.listers = append(skillHookRegistry.listers, fn)
}

func RegisterProfessionalSkillLister(fn ProfessionalSkillLister) {
	if fn == nil {
		return
	}
	skillHookRegistry.Lock()
	defer skillHookRegistry.Unlock()
	skillHookRegistry.professionalListers = append(skillHookRegistry.professionalListers, fn)
}

func RegisterRuntimeSkillConfigurer(fn RuntimeSkillConfigurer) {
	if fn == nil {
		return
	}
	skillHookRegistry.Lock()
	defer skillHookRegistry.Unlock()
	skillHookRegistry.configurers = append(skillHookRegistry.configurers, fn)
}

func RegisterSelectedSkillContextResolver(fn SelectedSkillContextResolver) {
	if fn == nil {
		return
	}
	skillHookRegistry.Lock()
	defer skillHookRegistry.Unlock()
	skillHookRegistry.resolvers = append(skillHookRegistry.resolvers, fn)
}

func RegisterAllSkillContextResolver(fn AllSkillContextResolver) {
	if fn == nil {
		return
	}
	skillHookRegistry.Lock()
	defer skillHookRegistry.Unlock()
	skillHookRegistry.allResolvers = append(skillHookRegistry.allResolvers, fn)
}

func additionalSkillMetadata(ctx context.Context) []*skills.SkillMetadata {
	skillHookRegistry.RLock()
	listers := append([]AdditionalSkillLister(nil), skillHookRegistry.listers...)
	skillHookRegistry.RUnlock()

	var result []*skills.SkillMetadata
	for _, lister := range listers {
		items, err := lister(ctx)
		if err != nil {
			logger.Warnf(ctx, "additional skill lister failed: %v", err)
			continue
		}
		result = append(result, items...)
	}
	return result
}

func professionalSkillMetadata(ctx context.Context) []*skills.SkillMetadata {
	skillHookRegistry.RLock()
	listers := append([]ProfessionalSkillLister(nil), skillHookRegistry.professionalListers...)
	skillHookRegistry.RUnlock()

	var result []*skills.SkillMetadata
	for _, lister := range listers {
		items, err := lister(ctx)
		if err != nil {
			logger.Warnf(ctx, "professional skill lister failed: %v", err)
			continue
		}
		result = append(result, items...)
	}
	return result
}

func configureRuntimeSkills(ctx context.Context, req *types.QARequest, agentConfig *types.AgentConfig, customAgent *types.CustomAgent) {
	skillHookRegistry.RLock()
	configurers := append([]RuntimeSkillConfigurer(nil), skillHookRegistry.configurers...)
	skillHookRegistry.RUnlock()

	for _, configurer := range configurers {
		if err := configurer(ctx, req, agentConfig, customAgent); err != nil {
			logger.Warnf(ctx, "runtime skill configurer failed: %v", err)
		}
	}
}

func selectedSkillContext(ctx context.Context, names []string) string {
	if len(names) == 0 {
		return ""
	}

	skillHookRegistry.RLock()
	resolvers := append([]SelectedSkillContextResolver(nil), skillHookRegistry.resolvers...)
	skillHookRegistry.RUnlock()

	parts := make([]string, 0, len(resolvers))
	for _, resolver := range resolvers {
		text, err := resolver(ctx, names)
		if err != nil {
			logger.Warnf(ctx, "selected skill context resolver failed: %v", err)
			continue
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func allSkillContext(ctx context.Context) string {
	skillHookRegistry.RLock()
	resolvers := append([]AllSkillContextResolver(nil), skillHookRegistry.allResolvers...)
	skillHookRegistry.RUnlock()

	parts := make([]string, 0, len(resolvers))
	for _, resolver := range resolvers {
		text, err := resolver(ctx)
		if err != nil {
			logger.Warnf(ctx, "all skill context resolver failed: %v", err)
			continue
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// SelectedSkillContext exposes the same selected-skill prompt context used by
// native AgentQA to custom runners. It is read-only and preserves the existing
// resolver registry behavior.
func SelectedSkillContext(ctx context.Context, names []string) string {
	return selectedSkillContext(ctx, names)
}

// LightweightSkillContext returns prompt-context guidance for lightweight
// skills. Chat-selected skills are always lightweight; agent-configured
// lightweight skills are merged here.
func LightweightSkillContext(ctx context.Context, mode string, names []string, chatNames []string) string {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "none"
	}
	if mode == "all" {
		all := allSkillContext(ctx)
		chat := selectedSkillContext(ctx, chatNames)
		if all != "" && chat != "" {
			return all + "\n\n" + chat
		}
		if all != "" {
			return all
		}
		return chat
	}
	merged := append([]string{}, names...)
	merged = append(merged, chatNames...)
	return selectedSkillContext(ctx, merged)
}

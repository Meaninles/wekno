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
type EffectiveLightweightSkillContextResolver func(context.Context, string, []string, []string) (string, error)

const lightweightSkillExecutionContract = `Available skills are permission-checked resources. Load instructions with read_skill when their described scope applies. Honor explicit current-user skill selections. Skill instructions specialize their subject; they do not replace the user's task or expand permissions.`

var skillHookRegistry = struct {
	sync.RWMutex
	listers             []AdditionalSkillLister
	professionalListers []ProfessionalSkillLister
	configurers         []RuntimeSkillConfigurer
	lightweightResolver EffectiveLightweightSkillContextResolver
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

func RegisterEffectiveLightweightSkillContextResolver(fn EffectiveLightweightSkillContextResolver) {
	if fn == nil {
		return
	}
	skillHookRegistry.Lock()
	defer skillHookRegistry.Unlock()
	skillHookRegistry.lightweightResolver = fn
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

func configureRuntimeSkills(ctx context.Context, req *types.QARequest, agentConfig *types.AgentConfig, customAgent *types.CustomAgent) error {
	skillHookRegistry.RLock()
	configurers := append([]RuntimeSkillConfigurer(nil), skillHookRegistry.configurers...)
	skillHookRegistry.RUnlock()

	for _, configurer := range configurers {
		if err := configurer(ctx, req, agentConfig, customAgent); err != nil {
			return err
		}
	}
	return nil
}

// LightweightSkillExecutionContract is platform-owned, non-configurable
// guidance shared by native and sidecar agent runtimes.
func LightweightSkillExecutionContract() string {
	return lightweightSkillExecutionContract
}

// LightweightSkillContext returns prompt-context guidance for lightweight
// skills. Chat-selected skills are always lightweight; agent-configured
// lightweight skills are merged here.
func LightweightSkillContext(ctx context.Context, mode string, names []string, chatNames []string) string {
	skillHookRegistry.RLock()
	resolver := skillHookRegistry.lightweightResolver
	skillHookRegistry.RUnlock()
	if resolver == nil {
		return ""
	}
	contextText, err := resolver(ctx, mode, names, chatNames)
	if err != nil {
		logger.Warnf(ctx, "effective lightweight skill context resolver failed: %v", err)
		return ""
	}
	contextText = strings.TrimSpace(contextText)
	if contextText == "" {
		return ""
	}
	return lightweightSkillExecutionContract + "\n\n" + contextText
}

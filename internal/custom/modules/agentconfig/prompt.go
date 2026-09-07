// Package agentconfig resolves runtime configuration independently of clients.
package agentconfig

import (
	"fmt"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"strings"
)

// NormalizePrompts stores a template reference, not a frozen copy of the
// template shown by an editor. An edited body is an explicit override and is
// preserved byte for byte. Resolution and validation still use one path.
func NormalizePrompts(cfg types.CustomAgentConfig, templates *config.PromptTemplatesConfig) (types.CustomAgentConfig, error) {
	if _, err := ResolvePrompts(cfg, templates); err != nil {
		return cfg, err
	}
	if templates == nil {
		return cfg, nil
	}
	canonical := func(id, body string, lists ...[]config.PromptTemplate) (string, string) {
		if body == "" {
			return id, body
		}
		for _, list := range lists {
			for _, item := range list {
				if (id == "" || id == item.ID) && body == item.Content {
					return item.ID, ""
				}
			}
		}
		return id, body
	}
	cfg.SystemPromptID, cfg.SystemPrompt = canonical(cfg.SystemPromptID, cfg.SystemPrompt, templates.SystemPrompt, templates.AgentSystemPrompt)

	return cfg, nil
}

// ResolvePrompts leaves stored configuration untouched. Explicit bodies win;
// references are validated even when overridden, so invalid IDs cannot hide.
func ResolvePrompts(cfg types.CustomAgentConfig, templates *config.PromptTemplatesConfig) (types.CustomAgentConfig, error) {
	if err := cfg.RetrievalBudget.Validate(); err != nil {
		return cfg, err
	}
	resolve := func(id, body, field string, lists ...[]config.PromptTemplate) (string, error) {
		if id == "" {
			return body, nil
		}
		for _, list := range lists {
			for _, item := range list {
				if item.ID == id {
					if strings.TrimSpace(body) != "" {
						return body, nil
					}
					if strings.TrimSpace(item.Content) == "" {
						return "", fmt.Errorf("%s %q has empty content", field, id)
					}
					return item.Content, nil
				}
			}
		}
		return "", fmt.Errorf("unknown %s %q", field, id)
	}
	if templates == nil {
		templates = &config.PromptTemplatesConfig{}
	}
	var err error
	cfg.SystemPrompt, err = resolve(cfg.SystemPromptID, cfg.SystemPrompt, "system_prompt_id", templates.SystemPrompt, templates.AgentSystemPrompt)
	if err != nil {
		return cfg, err
	}

	return cfg, err
}

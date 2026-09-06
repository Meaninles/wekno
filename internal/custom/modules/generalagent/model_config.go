package generalagent

import (
	"fmt"
	"github.com/Tencent/WeKnora/internal/types"
	"strings"
)

func runtimeLLMConfigFromModel(model *types.Model) (*LLMConfig, error) {
	if model == nil || !model.IsInteractiveChatModel() {
		return nil, fmt.Errorf("agent requires an interactive chat model")
	}
	adapter := strings.TrimSpace(model.Parameters.ExtraConfig["agent_runtime_adapter"])
	switch adapter {
	case "", "platform":
		return &LLMConfig{RuntimeAdapter: "platform", SupportsVision: model.Parameters.SupportsVision,
			ModelName: model.Name, BaseURL: model.Parameters.BaseURL, Provider: model.Parameters.Provider}, nil
	case "claude-sdk":
		// SDK is an explicit capability choice. Never derive it from a model name.
		if model.Parameters.Provider != "anthropic" {
			return nil, fmt.Errorf("claude-sdk requires an explicitly configured Anthropic provider")
		}
		config, err := generalClaudeLLMConfigFromModel(model)
		if err != nil {
			return nil, err
		}
		config.RuntimeAdapter = "claude-sdk"
		return config, nil
	default:
		return nil, fmt.Errorf("unsupported agent_runtime_adapter %q", adapter)
	}
}

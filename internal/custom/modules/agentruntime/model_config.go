package agentruntime

import (
	"fmt"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"strings"
)

func runtimeLLMConfigFromModel(model *types.Model) (*LLMConfig, error) {
	if model == nil || !model.IsInteractiveChatModel() {
		return nil, fmt.Errorf("agent requires an interactive chat model")
	}
	return runtimeProviderConfig(model)
}

func runtimeProviderConfig(model *types.Model) (*LLMConfig, error) {
	p := model.Parameters
	modelName := model.Name
	if remote := strings.TrimSpace(p.ExtraConfig["remote_model_name"]); remote != "" {
		modelName = remote
	}
	protocol := strings.TrimSpace(p.ExtraConfig["api_protocol"])
	if protocol == "" {
		protocol = "openai-chat"
		if p.Provider == "anthropic" {
			protocol = "anthropic"
		}
	}
	switch protocol {
	case "openai-chat", "openai-responses", "anthropic":
	default:
		return nil, fmt.Errorf("unsupported model API protocol %q", protocol)
	}
	return &LLMConfig{GenerationPolicy: strings.TrimSpace(p.ExtraConfig["generation_policy"]), Protocol: protocol, SupportsVision: p.SupportsVision, ModelName: modelName, BaseURL: p.BaseURL, APIKey: p.APIKey, Provider: p.Provider, Headers: p.CustomHeaders, ReasoningEffort: strings.TrimSpace(p.ExtraConfig["reasoning_effort"]), ThinkingControl: chat.EffectiveThinkingControl(&chat.ChatConfig{ModelName: modelName, Provider: p.Provider, BaseURL: p.BaseURL, ExtraConfig: p.ExtraConfig})}, nil
}

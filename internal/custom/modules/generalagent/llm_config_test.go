package generalagent

import (
	"testing"

	modelprovider "github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestDeriveGeneralClaudeEndpointKnownProviders(t *testing.T) {
	cases := []struct {
		name     string
		cfg      LLMConfig
		wantBase string
	}{
		{
			name: "anthropic provider keeps configured base",
			cfg: LLMConfig{
				ModelName: "claude-sonnet-4-5",
				BaseURL:   "https://api.anthropic.com",
				Provider:  string(modelprovider.ProviderAnthropic),
			},
			wantBase: "https://api.anthropic.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			deriveGeneralClaudeEndpoint(&cfg)
			if cfg.BaseURL != tc.wantBase {
				t.Fatalf("BaseURL = %q, want %q", cfg.BaseURL, tc.wantBase)
			}
			if cfg.Provider != string(modelprovider.ProviderAnthropic) {
				t.Fatalf("Provider = %q, want anthropic", cfg.Provider)
			}
		})
	}
}

func TestDeriveGeneralClaudeEndpointRequiresModelManagedAnthropicBase(t *testing.T) {
	cases := []LLMConfig{
		{
			ModelName: "gpt-4.1",
			BaseURL:   "https://api.openai.com/v1",
			Provider:  string(modelprovider.ProviderOpenAI),
		},
		{
			ModelName: "deepseek-reasoner",
			BaseURL:   "https://api.deepseek.com/v1",
			Provider:  string(modelprovider.ProviderDeepSeek),
		},
		{
			ModelName: "glm-4.6",
			BaseURL:   "https://open.bigmodel.cn/api/paas/v4",
			Provider:  string(modelprovider.ProviderZhipu),
		},
	}
	for _, tc := range cases {
		cfg := tc
		deriveGeneralClaudeEndpoint(&cfg)
		if cfg.BaseURL != "" {
			t.Fatalf("non-Anthropic model base URL should require explicit model-managed override, got %q", cfg.BaseURL)
		}
	}
}

func TestApplyGeneralClaudeOverridesAcceptsConfiguredAnthropicBaseURLWithoutAPIKey(t *testing.T) {
	cfg := &LLMConfig{
		ModelName: "local-qwen",
		BaseURL:   "http://local-openai-compatible/v1",
		Provider:  string(modelprovider.ProviderGeneric),
	}

	override := applyGeneralClaudeOverrides(cfg, map[string]string{
		"general_agent_claude_base_url": "http://model-config-anthropic-compatible",
	})
	if !override.baseConfigured {
		t.Fatalf("baseConfigured = false, want true")
	}
	if cfg.BaseURL != "http://model-config-anthropic-compatible" {
		t.Fatalf("BaseURL = %q, want model config value", cfg.BaseURL)
	}
	if cfg.Provider != string(modelprovider.ProviderAnthropic) {
		t.Fatalf("Provider = %q, want anthropic", cfg.Provider)
	}
}

func TestApplyGeneralClaudeOverridesUsesModelConfigOnly(t *testing.T) {
	t.Setenv("CUSTOM_GENERAL_AGENT_CLAUDE_BASE_URL", "http://env-anthropic-compatible")
	cfg := &LLMConfig{
		ModelName: "local-qwen",
		BaseURL:   "http://local-openai-compatible/v1",
		Provider:  string(modelprovider.ProviderGeneric),
	}

	override := applyGeneralClaudeOverrides(cfg, map[string]string{})
	if override.baseConfigured {
		t.Fatalf("baseConfigured = true, want false")
	}
	if cfg.BaseURL != "http://local-openai-compatible/v1" {
		t.Fatalf("BaseURL = %q, want original model value", cfg.BaseURL)
	}
}

func TestGeneralClaudeLLMConfigFromModelUsesModelAPIKey(t *testing.T) {
	cfg, err := generalClaudeLLMConfigFromModel(&types.Model{
		Type: types.ModelTypeKnowledgeQA,
		Name: "/models/Qwen3.6-27B",
		Parameters: types.ModelParameters{
			BaseURL:  "http://10.0.11.37:30001/v1",
			APIKey:   "sk-model-configured-key",
			Provider: string(modelprovider.ProviderGeneric),
			ExtraConfig: map[string]string{
				"general_agent_claude_base_url": "http://10.0.11.37:30001",
			},
		},
	})
	if err != nil {
		t.Fatalf("generalClaudeLLMConfigFromModel error: %v", err)
	}
	if cfg.BaseURL != "http://10.0.11.37:30001" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.APIKey != "sk-model-configured-key" {
		t.Fatalf("APIKey = %q, want model-managed key", cfg.APIKey)
	}
	if cfg.AuthType != generalClaudeAuthAPIKey {
		t.Fatalf("AuthType = %q, want %q", cfg.AuthType, generalClaudeAuthAPIKey)
	}
}

func TestGeneralClaudeLLMConfigFromModelAllowsNoAPIKeyWithModelBaseURL(t *testing.T) {
	cfg, err := generalClaudeLLMConfigFromModel(&types.Model{
		Type: types.ModelTypeKnowledgeQA,
		Name: "/models/Qwen3.6-27B",
		Parameters: types.ModelParameters{
			BaseURL:  "http://10.0.11.37:30001/v1",
			Provider: string(modelprovider.ProviderGeneric),
			ExtraConfig: map[string]string{
				"general_agent_claude_base_url": "http://10.0.11.37:30001",
			},
		},
	})
	if err != nil {
		t.Fatalf("generalClaudeLLMConfigFromModel error: %v", err)
	}
	if cfg.APIKey != "" {
		t.Fatalf("APIKey = %q, want empty API key for local no-auth endpoint", cfg.APIKey)
	}
	if cfg.AuthType != generalClaudeAuthHelper {
		t.Fatalf("AuthType = %q, want %q", cfg.AuthType, generalClaudeAuthHelper)
	}
	if cfg.APIKeyHelper == "" {
		t.Fatalf("APIKeyHelper is empty, want helper for local no-auth endpoint")
	}
}

func TestGeneralClaudeLLMConfigFromModelIgnoresEnvBaseURL(t *testing.T) {
	t.Setenv("CUSTOM_GENERAL_AGENT_CLAUDE_BASE_URL", "http://env-anthropic-compatible")
	_, err := generalClaudeLLMConfigFromModel(&types.Model{
		Type: types.ModelTypeKnowledgeQA,
		Name: "/models/Generic",
		Parameters: types.ModelParameters{
			BaseURL:  "http://generic-openai-compatible/v1",
			APIKey:   "sk-model-configured-key",
			Provider: string(modelprovider.ProviderGeneric),
		},
	})
	if err == nil {
		t.Fatalf("expected explicit model-managed Anthropic base URL to be required for generic providers")
	}
}

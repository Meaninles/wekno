package generalagent

import (
	"testing"

	modelprovider "github.com/Tencent/WeKnora/internal/models/provider"
)

func TestDeriveGeneralClaudeEndpointKnownProviders(t *testing.T) {
	cases := []struct {
		name     string
		cfg      LLMConfig
		wantBase string
	}{
		{
			name: "zhipu provider",
			cfg: LLMConfig{
				ModelName: "glm-4.6",
				BaseURL:   "https://open.bigmodel.cn/api/paas/v4",
				Provider:  string(modelprovider.ProviderZhipu),
			},
			wantBase: zhipuAnthropicBaseURL,
		},
		{
			name: "glm model name",
			cfg: LLMConfig{
				ModelName: "glm-4-air",
				BaseURL:   "https://example.com/openai/v1",
				Provider:  string(modelprovider.ProviderGeneric),
			},
			wantBase: zhipuAnthropicBaseURL,
		},
		{
			name: "deepseek provider",
			cfg: LLMConfig{
				ModelName: "deepseek-reasoner",
				BaseURL:   "https://api.deepseek.com/v1",
				Provider:  string(modelprovider.ProviderDeepSeek),
			},
			wantBase: deepSeekAnthropicBaseURL,
		},
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

func TestDeriveGeneralClaudeEndpointDoesNotUseGenericOpenAIBaseURL(t *testing.T) {
	cfg := &LLMConfig{
		ModelName: "gpt-4.1",
		BaseURL:   "https://api.openai.com/v1",
		Provider:  string(modelprovider.ProviderOpenAI),
	}

	deriveGeneralClaudeEndpoint(cfg)

	if cfg.BaseURL != "" {
		t.Fatalf("generic OpenAI-compatible base URL should require explicit Claude override, got %q", cfg.BaseURL)
	}
}

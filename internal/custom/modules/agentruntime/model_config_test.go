package agentruntime

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestReasoningFormatIsAnExplicitProviderCapability(t *testing.T) {
	for _, name := range []string{"any-model", "different-provider-model"} {
		model := &types.Model{Name: name, Parameters: types.ModelParameters{ExtraConfig: map[string]string{}}}
		cfg, err := runtimeProviderConfig(model)
		require.NoError(t, err)
		require.Equal(t, "native", cfg.ReasoningFormat)
		model.Parameters.ExtraConfig["reasoning_format"] = "think-tags"
		cfg, err = runtimeProviderConfig(model)
		require.NoError(t, err)
		require.Equal(t, "think-tags", cfg.ReasoningFormat)
		model.Parameters.ExtraConfig["api_protocol"] = "anthropic"
		_, err = runtimeProviderConfig(model)
		require.Error(t, err)
		model.Parameters.ExtraConfig["api_protocol"] = "openai-chat"
		model.Parameters.ExtraConfig["reasoning_format"] = "unknown"
		_, err = runtimeProviderConfig(model)
		require.Error(t, err)
	}
}

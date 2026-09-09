package agentruntime

import (
	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMCPSchedulingHonorsExplicitReadHintAndApproval(t *testing.T) {
	for _, tc := range []struct {
		hint     *bool
		approval bool
		parallel bool
	}{
		{nil, false, false}, {boolValue(false), false, false}, {boolValue(true), false, true}, {boolValue(true), true, false},
	} {
		registry := agenttools.NewToolRegistry()
		tool := agenttools.NewMCPTool(&types.MCPService{Name: "external"}, &types.MCPTool{Name: "read", ReadOnlyHint: tc.hint, RequireApproval: tc.approval}, nil, nil, 0)
		registry.RegisterTool(tool)
		specs := runtimeToolSpecs(registry)
		require.Len(t, specs, 1)
		require.Equal(t, tc.parallel, specs[0].IsConcurrencySafe)
	}
}

func TestInputToolsAreScopedToDirectMatchingInputs(t *testing.T) {
	config := &types.AgentConfig{VLMModelID: "vision-model", ASRModelID: "asr-model"}
	direct := []OriginalInputFileSpec{
		{ID: "image-1", Source: types.OriginalInputSourceChatUpload, FileType: "png"},
		{ID: "audio-1", Source: types.OriginalInputSourceChatUpload, FileType: "wav"},
	}
	specs := runtimeToolSpecsWithInputs(nil, direct, config)
	names := make(map[string]bool, len(specs))
	for _, spec := range specs {
		names[spec.Name] = true
	}
	require.True(t, names[ToolInspectInputImage])
	require.True(t, names[ToolTranscribeInputFile])

	selectedKnowledgeFile := []OriginalInputFileSpec{
		{ID: "knowledge-image", Source: types.OriginalInputSourceSelectedKnowledge, FileType: "png"},
		{ID: "knowledge-audio", Source: types.OriginalInputSourceSelectedKnowledge, FileType: "wav"},
	}
	selectedSpecs := runtimeToolSpecsWithInputs(nil, selectedKnowledgeFile, config)
	require.Empty(t, selectedSpecs)
	require.Empty(t, runtimeToolSpecsWithInputs(nil, nil, config))
}

func boolValue(value bool) *bool { return &value }

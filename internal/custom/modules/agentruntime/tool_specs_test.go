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
func boolValue(value bool) *bool { return &value }

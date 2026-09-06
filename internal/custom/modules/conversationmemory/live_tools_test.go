package conversationmemory

import (
	"context"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestRepairLiveToolBudgetRetainsRolesPairsAndReadableOriginal(t *testing.T) {
	ctx := WithLiveTools(context.Background())
	text := strings.Repeat("long exact tool output\n", 1000)
	input := []chat.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "current user request"},
		{Role: "assistant", ReasoningContent: "provider reasoning", ToolCalls: []chat.ToolCall{{ID: "c1", Function: chat.FunctionCall{Name: "lookup", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: text},
	}
	out, err := BoundMessages(ctx, input, readableTools(), 800, 100)
	require.NoError(t, err)
	require.Len(t, out, 4)
	require.Equal(t, input[:3], out[:3])
	require.Equal(t, "c1", out[3].ToolCallID)
	require.Contains(t, out[3].Content, "archived_tool_result")
	original, ok := LiveToolsFromContext(ctx).Read("c1")
	require.True(t, ok)
	require.Equal(t, text, original)
	require.Equal(t, text, input[3].Content)
	_, ok = LiveToolsFromContext(WithLiveTools(context.Background())).Read("c1")
	require.False(t, ok)
	again, err := BoundMessages(ctx, input, readableTools(), 800, 100)
	require.NoError(t, err)
	require.Equal(t, out, again)
}
func TestRepairLiveToolBudgetDoesNotSummarizeUserInput(t *testing.T) {
	ctx := WithLiveTools(context.Background())
	_, err := BoundMessages(ctx, []chat.Message{{Role: "system", Content: "system"}, {Role: "user", Content: strings.Repeat("exact user input", 1000)}}, readableTools(), 500, 100)
	require.ErrorContains(t, err, "user context")
}

func readableTools() []chat.Tool {
	return []chat.Tool{{Function: chat.FunctionDef{Name: "read_conversation"}}}
}

func TestProactiveArchivalKeepsEvidenceImagesAndProtocol(t *testing.T) {
	ctx := WithLiveTools(context.Background())
	original := strings.Repeat("制度原文\r\n", 8000)
	input := []chat.Message{{Role: "user", Content: "原始限定条件"},
		{Role: "assistant", ReasoningContent: "opaque reasoning", ToolCalls: []chat.ToolCall{{ID: "source", Function: chat.FunctionCall{Name: "search", Arguments: `{}`}}}},
		{Role: "tool", Name: "search", ToolCallID: "source", Content: original, Images: []string{"image-fixture"}}}
	out, err := BoundMessages(ctx, input, readableTools(), 0, 0)
	require.NoError(t, err)
	require.Equal(t, input[:2], out[:2])
	require.Equal(t, input[2].Images, out[2].Images)
	require.Less(t, len(out[2].Content), len(original))
	exact, ok := LiveToolsFromContext(ctx).Read("source")
	require.True(t, ok)
	require.Equal(t, original, exact)
	input[2].Name = "read_conversation"
	out, err = BoundMessages(ctx, input, readableTools(), 0, 0)
	require.NoError(t, err)
	require.Equal(t, input, out)
	out, err = BoundMessages(ctx, input, nil, 0, 0)
	require.NoError(t, err)
	require.Equal(t, input, out)
	_, err = BoundMessages(ctx, input, nil, 100, 0)
	require.ErrorContains(t, err, "readable tool archive")
}

func TestRecoveryIsOncePerRunAndDoesNotRewritePriorMessages(t *testing.T) {
	ctx := WithLiveTools(context.Background())
	original := []chat.Message{{Role: "user", Content: "exact task"}, {Role: "assistant", Content: "previous"}}
	resumed, ok := RecoverResponse(ctx, original, ResponseRecoveryReason("length", "", true))
	require.True(t, ok)
	require.Equal(t, original, resumed[:len(original)])
	_, ok = RecoverResponse(ctx, resumed, "output_limit")
	require.False(t, ok)
	for _, reason := range []string{"", "tool_calls", "error"} {
		require.Empty(t, ResponseRecoveryReason(reason, "", false))
	}
	require.Empty(t, ResponseRecoveryReason("stop", "", true))
	canceled, cancel := context.WithCancel(WithLiveTools(context.Background()))
	cancel()
	_, ok = RecoverResponse(canceled, original, "output_limit")
	require.False(t, ok)
}

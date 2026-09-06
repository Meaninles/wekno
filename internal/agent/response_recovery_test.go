package agent

import (
	"context"
	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestResponseRecoveryPersistsProtocolAndDoesNotRaiseBudget(t *testing.T) {
	partial := types.LLMToolCall{ID: "unfinished", Function: types.FunctionCall{Name: "write_file", Arguments: `{"content":"partial`}}
	model := &mockChat{responses: []mockResponse{
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "length", ToolCalls: []types.LLMToolCall{partial}}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Content: "complete", Done: true, FinishReason: "stop"}}},
		{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "length", ToolCalls: []types.LLMToolCall{partial}}}},
	}}
	engine := newTestEngine(t, model, func(config *types.AgentConfig) { config.MaxCompletionTokens = 8192 })
	ctx := conversationmemory.WithLiveTools(context.Background())
	messages := []chat.Message{{Role: "user", Content: "exact request"},
		{Role: "assistant", ReasoningContent: "provider state", ToolCalls: []chat.ToolCall{{ID: "done", Function: chat.FunctionCall{Name: "read_file", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "done", Name: "read_file", Content: "exact evidence"}}
	original := append([]chat.Message(nil), messages...)
	response, err := engine.callLLMWithRetry(ctx, &messages, nil, &types.AgentState{}, "exact request", 0, "session")
	require.NoError(t, err)
	require.Equal(t, "complete", response.Content)
	require.Equal(t, original, messages[:len(original)])
	require.Len(t, messages, len(original)+1)
	require.Equal(t, messages, model.messages[1])
	require.Equal(t, model.options[0].MaxCompletionTokens, model.options[1].MaxCompletionTokens)
	require.Equal(t, 8192, model.options[1].MaxCompletionTokens)
	_, err = engine.callLLMWithRetry(ctx, &messages, nil, &types.AgentState{}, "exact request", 1, "session")
	require.Error(t, err)
	require.Equal(t, 3, model.callCount)
	require.Len(t, messages, len(original)+1)
}

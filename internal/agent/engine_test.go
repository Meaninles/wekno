package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock: chat.Chat
// ---------------------------------------------------------------------------

type mockResponse struct {
	chunks []types.StreamResponse
}

type mockChat struct {
	mu        sync.Mutex
	responses []mockResponse
	callCount int
	options   []*chat.ChatOptions
	messages  [][]chat.Message
}

func (m *mockChat) ChatStream(_ context.Context, messages []chat.Message, opts *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.callCount >= len(m.responses) {
		return nil, fmt.Errorf("unexpected ChatStream call #%d (only %d responses prepared)", m.callCount, len(m.responses))
	}
	resp := m.responses[m.callCount]
	m.callCount++
	m.messages = append(m.messages, append([]chat.Message(nil), messages...))
	if opts == nil {
		m.options = append(m.options, nil)
	} else {
		copyOpts := *opts
		m.options = append(m.options, &copyOpts)
	}

	ch := make(chan types.StreamResponse, len(resp.chunks))
	for _, chunk := range resp.chunks {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

func (m *mockChat) Chat(_ context.Context, _ []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockChat) GetModelName() string { return "mock-model" }
func (m *mockChat) GetModelID() string   { return "mock-id" }

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type testEngineOption func(*types.AgentConfig)

func withMaxIterations(n int) testEngineOption {
	return func(cfg *types.AgentConfig) {
		cfg.MaxIterations = n
	}
}

func newTestEngine(t *testing.T, chatModel chat.Chat, opts ...testEngineOption) *AgentEngine {
	t.Helper()
	cfg := &types.AgentConfig{
		MaxIterations: 10,
		Temperature:   0.7,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	engine := NewAgentEngine(
		cfg,
		chatModel,
		nil,
		event.NewEventBus(),
		nil,
		nil,
		"test-session",
		"",
	)
	require.NotNil(t, engine, "NewAgentEngine returned nil (agenttoken.NewEstimator failed?)")
	return engine
}

func emptyMessages() []chat.Message {
	return []chat.Message{
		{Role: "system", Content: "You are a test agent."},
		{Role: "user", Content: "test query"},
	}
}

func emptyTools() []chat.Tool {
	return nil
}

func TestBuildSystemPromptAppendsLightweightSkillContextAfterAgentBaseline(t *testing.T) {
	engine := &AgentEngine{
		config: &types.AgentConfig{
			LightweightSkillContext: "Lightweight skill execution contract:\n[本轮有效轻量 Skills]\n制度助手：你是茅小规。",
		},
		systemPromptTemplate: "Generic agent baseline.",
	}

	prompt := engine.buildSystemPrompt(context.Background())
	require.Contains(t, prompt, "Generic agent baseline.")
	require.Contains(t, prompt, "制度助手：你是茅小规。")
	require.Less(t, strings.Index(prompt, "Generic agent baseline."), strings.Index(prompt, "制度助手：你是茅小规。"),
		"platform-resolved lightweight skills must be appended after the generic agent baseline")
}

func TestBuildSystemPromptAppendsDurableUserContextWithoutReplacingBaseline(t *testing.T) {
	engine := &AgentEngine{
		config: &types.AgentConfig{
			DurableUserContext: "earlier_user_message_01: project foundation",
		},
		systemPromptTemplate: "Full native RAG baseline.",
	}

	prompt := engine.buildSystemPrompt(context.Background())
	require.Contains(t, prompt, "Full native RAG baseline.")
	require.Contains(t, prompt, "project foundation")
	require.Contains(t, prompt, "DIALOGUE_CONTEXT")
	require.Less(t, strings.Index(prompt, "Full native RAG baseline."), strings.Index(prompt, "project foundation"))
}

// ---------------------------------------------------------------------------
// TC1: Empty content + stop → should NOT complete with empty FinalAnswer
// ---------------------------------------------------------------------------

func TestExecuteLoop_EmptyContentWithStop_ShouldNotCompleteWithEmpty(t *testing.T) {
	// Simulate: LLM returns empty content with no tool calls (natural stop).
	// The stream closes with no content chunks → streamLLMToEventBus returns fullContent="".
	// streamThinkingToEventBus wraps it as ChatResponse{Content:"", FinishReason:"stop"}.
	// analyzeResponse() returns verdict{isDone:true, finalAnswer:""} → BUG: empty answer.
	//
	// Prepare 3 responses for initial attempt + 2 retries (after fix).
	mock := &mockChat{
		responses: []mockResponse{
			{chunks: []types.StreamResponse{{Done: true}}},
			{chunks: []types.StreamResponse{{Done: true}}},
			{chunks: []types.StreamResponse{{Done: true}}},
		},
	}

	engine := newTestEngine(t, mock)
	state := &types.AgentState{}
	ctx := context.Background()

	_, err := engine.executeLoop(ctx, state, "test query", emptyMessages(), emptyTools(), "sess-1", "msg-1")

	assert.Error(t, err)
	assert.False(t, state.IsComplete)
	assert.Empty(t, state.FinalAnswer)
	assert.Equal(t, 1, mock.callCount)
}

// ---------------------------------------------------------------------------
// TC2: Non-empty content + stop → normal completion (regression guard)
// ---------------------------------------------------------------------------

func TestExecuteLoop_NonEmptyContentWithStop_ShouldComplete(t *testing.T) {
	mock := &mockChat{
		responses: []mockResponse{
			{chunks: []types.StreamResponse{
				{ResponseType: types.ResponseTypeAnswer, Content: "Here is my answer", Done: true, FinishReason: "stop"},
			}},
		},
	}

	engine := newTestEngine(t, mock)
	state := &types.AgentState{}
	ctx := context.Background()

	_, err := engine.executeLoop(ctx, state, "test query", emptyMessages(), emptyTools(), "sess-1", "msg-1")

	assert.NoError(t, err)
	assert.True(t, state.IsComplete)
	assert.Equal(t, "Here is my answer", state.FinalAnswer)
}

// ---------------------------------------------------------------------------
// TC4: Empty → retry with nudge → non-empty → success
// ---------------------------------------------------------------------------

func TestStreamThinkingToEventBus_PropagatesFinishReason(t *testing.T) {
	tests := []struct {
		name         string
		finishReason string
		wantReason   string
	}{
		{"stop", "stop", "stop"},
		{"tool_calls", "tool_calls", "tool_calls"},
		{"length", "length", "length"},
		{"missing_finish_reason", "", ""}, // preserve missing protocol metadata
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockChat{
				responses: []mockResponse{
					{chunks: []types.StreamResponse{
						{ResponseType: types.ResponseTypeAnswer, Content: "test content", Done: true, FinishReason: tt.finishReason},
					}},
				},
			}

			engine := newTestEngine(t, mock)
			ctx := context.Background()
			msgs := []chat.Message{{Role: "user", Content: "test"}}
			tools := []chat.Tool{}

			resp, err := engine.streamThinkingToEventBus(ctx, msgs, tools, 0, "sess-1")

			assert.NoError(t, err)
			assert.Equal(t, tt.wantReason, resp.FinishReason)
		})
	}
}

func TestStreamThinkingToEventBus_WithholdsOutputLimitedDraft(t *testing.T) {
	mock := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer,
		Content:      "This sentence is cut off because",
		Done:         true,
		FinishReason: "length",
	}}}}}
	engine := newTestEngine(t, mock)
	var streamed string
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		if data, ok := evt.Data.(event.AgentFinalAnswerData); ok {
			streamed += data.Content
		}
		return nil
	})

	resp, err := engine.streamThinkingToEventBus(
		context.Background(), emptyMessages(), emptyTools(), 0, "sess-length",
	)

	require.NoError(t, err)
	assert.Equal(t, "length", resp.FinishReason)
	assert.Equal(t, "This sentence is cut off because", resp.Content)
	assert.False(t, resp.AnswerStreamed)
	assert.Empty(t, streamed, "a provider-truncated draft must never reach the answer stream")
}

func TestStreamThinkingToEventBus_RoutesReasoningAndAnswerSeparately(t *testing.T) {
	mock := &mockChat{
		responses: []mockResponse{
			{chunks: []types.StreamResponse{
				{ResponseType: types.ResponseTypeThinking, Content: "let me reason"},
				{ResponseType: types.ResponseTypeThinking, Content: "", Done: true},
				{ResponseType: types.ResponseTypeAnswer, Content: "The answer "},
				{ResponseType: types.ResponseTypeAnswer, Content: "is 42.", Done: true, FinishReason: "stop"},
			}},
		},
	}

	engine := newTestEngine(t, mock)
	var thoughts, answers string
	engine.eventBus.On(event.EventAgentThought, func(_ context.Context, evt event.Event) error {
		if d, ok := evt.Data.(event.AgentThoughtData); ok {
			thoughts += d.Content
		}
		return nil
	})
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		if d, ok := evt.Data.(event.AgentFinalAnswerData); ok {
			answers += d.Content
		}
		return nil
	})

	resp, err := engine.streamThinkingToEventBus(context.Background(),
		emptyMessages(), emptyTools(), 0, "sess-1")
	require.NoError(t, err)

	assert.Equal(t, "let me reason", thoughts, "reasoning_content must stream to thought events")
	assert.Equal(t, "The answer is 42.", answers, "plain answer content must stream live to final-answer events")
	assert.True(t, resp.AnswerStreamed, "AnswerStreamed must be set when answer text was streamed live")
	assert.NotEmpty(t, resp.AnswerEventID, "AnswerEventID must identify the live answer stream")
}

func TestStreamThinkingToEventBus_DoesNotFlushToolPreambleAfterToolEvent(t *testing.T) {
	mock := &mockChat{
		responses: []mockResponse{{chunks: []types.StreamResponse{
			{ResponseType: types.ResponseTypeAnswer, Content: "I will search first."},
			{
				ResponseType: types.ResponseTypeToolCall,
				ToolCalls: []types.LLMToolCall{{
					ID: "call-1", Type: "function",
					Function: types.FunctionCall{Name: "lookup", Arguments: `{}`},
				}},
				Done: true, FinishReason: "tool_calls",
			},
		}}},
	}

	engine := newTestEngine(t, mock)
	var answers string
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		if data, ok := evt.Data.(event.AgentFinalAnswerData); ok {
			answers += data.Content
		}
		return nil
	})

	resp, err := engine.streamThinkingToEventBus(
		context.Background(), emptyMessages(), emptyTools(), 0, "sess-1",
	)
	require.NoError(t, err)
	assert.Empty(t, answers)
	assert.False(t, resp.AnswerStreamed)
	assert.Equal(t, "I will search first.", resp.Content)
	require.Len(t, resp.ToolCalls, 1)
}

// TestStreamThinkingToEventBus_SplitsInlineThinkBlock verifies that models which
// embed reasoning inline as <think>…</think> in the content channel still have
// their reasoning routed to thought events and only the real answer streamed to
// the final-answer area.
func TestExecuteLoop_NaturalStop_DoesNotDuplicateAnswer(t *testing.T) {
	mock := &mockChat{
		responses: []mockResponse{
			{chunks: []types.StreamResponse{
				{ResponseType: types.ResponseTypeAnswer, Content: "Hello "},
				{ResponseType: types.ResponseTypeAnswer, Content: "world", Done: true, FinishReason: "stop"},
			}},
		},
	}

	engine := newTestEngine(t, mock)
	var answerContent string
	var doneCount int
	engine.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		if d, ok := evt.Data.(event.AgentFinalAnswerData); ok {
			answerContent += d.Content
			if d.Done {
				doneCount++
			}
		}
		return nil
	})

	state := &types.AgentState{}
	_, err := engine.executeLoop(context.Background(), state, "test query",
		emptyMessages(), emptyTools(), "sess-1", "msg-1")
	require.NoError(t, err)

	assert.True(t, state.IsComplete)
	assert.Equal(t, "Hello world", state.FinalAnswer)
	assert.Equal(t, "Hello world", answerContent,
		"answer content must be emitted exactly once (streamed live, not re-emitted by the natural-stop branch)")
	assert.GreaterOrEqual(t, doneCount, 1, "a Done marker must close the answer stream")
}

func TestRepairTerminalFailureIsNotSyntheticSuccess(t *testing.T) {
	for _, reason := range []string{"length", "", "stop"} {
		model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: reason}}}}}
		engine := newTestEngine(t, model)
		state := &types.AgentState{}
		messages := emptyMessages()
		_, err := engine.callLLMWithRetry(context.Background(), &messages, nil, state, "task", 0, "session")
		require.Error(t, err)
		require.Equal(t, 1, model.callCount)
		require.False(t, state.IsComplete)
	}
}
func TestModelFailureAfterToolEvidenceDoesNotInvokeAnotherGenerator(t *testing.T) {
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{ResponseType: types.ResponseTypeAnswer, Done: true, FinishReason: "length"}}}}}
	engine := newTestEngine(t, model)
	state := &types.AgentState{RoundSteps: []types.AgentStep{{ToolCalls: []types.ToolCall{{ID: "source-call", Name: "knowledge_search", Result: &types.ToolResult{Success: true, Output: "actual source"}}}}}}
	messages := emptyMessages()
	_, err := engine.callLLMWithRetry(context.Background(), &messages, nil, state, "task", 1, "session")
	require.Error(t, err)
	require.Equal(t, 1, model.callCount)
	require.False(t, state.IsComplete)
	require.Equal(t, "actual source", state.RoundSteps[0].ToolCalls[0].Result.Output)
}

func TestExhaustedLoopDoesNotGenerateWithoutTools(t *testing.T) {
	model := &mockChat{}
	engine := newTestEngine(t, model, withMaxIterations(1))
	state := &types.AgentState{CurrentRound: 1, RoundSteps: []types.AgentStep{{ToolCalls: []types.ToolCall{{ID: "call", Name: "knowledge_search", Result: &types.ToolResult{Success: true, Output: "source"}}}}}}
	_, err := engine.executeLoop(context.Background(), state, "task", emptyMessages(), emptyTools(), "session", "message")
	require.ErrorContains(t, err, "configured limit")
	require.Zero(t, model.callCount)
	require.False(t, state.IsComplete)
	require.Empty(t, state.FinalAnswer)
}

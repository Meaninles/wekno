package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type recordingStreamManager struct {
	events []interfaces.StreamEvent
}

func (m *recordingStreamManager) AppendEvent(ctx context.Context, sessionID, messageID string, evt interfaces.StreamEvent) error {
	m.events = append(m.events, evt)
	return nil
}

func (m *recordingStreamManager) GetEvents(ctx context.Context, sessionID, messageID string, fromOffset int) ([]interfaces.StreamEvent, int, error) {
	if fromOffset >= len(m.events) {
		return nil, len(m.events), nil
	}
	return m.events[fromOffset:], len(m.events), nil
}

func TestHandleToolCallPreservesAnswerForPostAnswerArtifact(t *testing.T) {
	stream := &recordingStreamManager{}
	msg := &types.Message{ID: "assistant-1", SessionID: "session-1"}
	handler := NewAgentStreamHandler(
		context.Background(),
		"session-1",
		"assistant-1",
		"request-1",
		time.Time{},
		msg,
		stream,
		event.NewEventBus(),
	)

	if err := handler.handleFinalAnswer(context.Background(), event.Event{
		ID:   "answer-1",
		Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: "final report summary", Done: true},
	}); err != nil {
		t.Fatalf("handleFinalAnswer returned error: %v", err)
	}
	if handler.finalAnswer != "final report summary" {
		t.Fatalf("finalAnswer before tool call = %q", handler.finalAnswer)
	}

	if err := handler.handleToolCall(context.Background(), event.Event{
		ID:   "artifact-tool",
		Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{
			ToolCallID:     "artifact-1",
			ToolName:       "create_artifact",
			Arguments:      map[string]interface{}{"secret": "do not send"},
			Iteration:      1,
			PreserveAnswer: true,
		},
	}); err != nil {
		t.Fatalf("handleToolCall returned error: %v", err)
	}

	if handler.finalAnswer != "final report summary" {
		t.Fatalf("finalAnswer after preserve-answer tool call = %q, want original answer", handler.finalAnswer)
	}
	if len(stream.events) < 2 {
		t.Fatalf("stream events = %d, want at least 2", len(stream.events))
	}
	gotPreserve, _ := stream.events[len(stream.events)-1].Data["preserve_answer"].(bool)
	if !gotPreserve {
		t.Fatalf("tool_call stream metadata preserve_answer = %v, want true", gotPreserve)
	}
	if _, ok := stream.events[len(stream.events)-1].Data["arguments"]; ok {
		t.Fatalf("tool_call arguments leaked to stream metadata: %#v", stream.events[len(stream.events)-1].Data)
	}
}

func TestHandleToolCallSupersedesPreambleByDefault(t *testing.T) {
	stream := &recordingStreamManager{}
	handler := NewAgentStreamHandler(
		context.Background(),
		"session-1",
		"assistant-1",
		"request-1",
		time.Time{},
		&types.Message{ID: "assistant-1", SessionID: "session-1"},
		stream,
		event.NewEventBus(),
	)

	if err := handler.handleFinalAnswer(context.Background(), event.Event{
		ID:   "answer-1",
		Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: "let me search first", Done: true},
	}); err != nil {
		t.Fatalf("handleFinalAnswer returned error: %v", err)
	}
	if err := handler.handleToolCall(context.Background(), event.Event{
		ID:   "tool-1",
		Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{
			ToolCallID: "tool-1",
			ToolName:   "web_search",
			Iteration:  1,
		},
	}); err != nil {
		t.Fatalf("handleToolCall returned error: %v", err)
	}

	if handler.finalAnswer != "" {
		t.Fatalf("finalAnswer after normal tool call = %q, want empty superseded preamble", handler.finalAnswer)
	}
}

func TestHandleCompleteReplacesExistingAssistantContent(t *testing.T) {
	stream := &recordingStreamManager{}
	msg := &types.Message{
		ID:        "assistant-1",
		SessionID: "session-1",
		Content:   "streamed final answer",
	}
	handler := NewAgentStreamHandler(
		context.Background(),
		"session-1",
		"assistant-1",
		"request-1",
		time.Time{},
		msg,
		stream,
		event.NewEventBus(),
	)

	if err := handler.handleFinalAnswer(context.Background(), event.Event{
		ID:   "answer-1",
		Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: "streamed final answer", Done: true},
	}); err != nil {
		t.Fatalf("handleFinalAnswer returned error: %v", err)
	}
	if err := handler.handleComplete(context.Background(), event.Event{
		ID:   "complete-1",
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{
			MessageID:   "assistant-1",
			FinalAnswer: "streamed final answer",
		},
	}); err != nil {
		t.Fatalf("handleComplete returned error: %v", err)
	}

	if msg.Content != "streamed final answer" {
		t.Fatalf("assistant content = %q, want single final answer", msg.Content)
	}
	if len(stream.events) == 0 {
		t.Fatalf("stream events = 0, want complete event")
	}
	complete := stream.events[len(stream.events)-1]
	if complete.Type != types.ResponseTypeComplete {
		t.Fatalf("last stream event type = %s, want complete", complete.Type)
	}
	if got := complete.Data["final_answer"]; got != "streamed final answer" {
		t.Fatalf("complete final_answer = %q, want streamed final answer", got)
	}
	refs, ok := complete.Data["knowledge_references"].([]*types.SearchResult)
	if !ok || len(refs) != 0 {
		t.Fatalf("complete knowledge_references = %#v, want authoritative empty slice", complete.Data["knowledge_references"])
	}
}

func TestHandleAgentProgressAppendsVisibleProgressEvent(t *testing.T) {
	stream := &recordingStreamManager{}
	handler := NewAgentStreamHandler(
		context.Background(),
		"session-1",
		"assistant-1",
		"request-1",
		time.Time{},
		&types.Message{ID: "assistant-1", SessionID: "session-1"},
		stream,
		event.NewEventBus(),
	)

	if err := handler.handleAgentProgress(context.Background(), event.Event{
		ID:   "evt-1",
		Type: event.EventAgentProgress,
		Data: event.AgentProgressData{
			Content:    "正在执行命令",
			ToolName:   "Bash",
			ToolCallID: "toolu-1",
			Phase:      "start",
		},
	}); err != nil {
		t.Fatalf("handleAgentProgress returned error: %v", err)
	}

	if len(stream.events) != 1 {
		t.Fatalf("stream events = %d, want 1", len(stream.events))
	}
	got := stream.events[0]
	if got.Type != types.ResponseTypeAgentProgress {
		t.Fatalf("stream event type = %s, want agent_progress", got.Type)
	}
	if got.Content != "正在执行命令" || got.Done {
		t.Fatalf("stream progress content/done = (%q, %v)", got.Content, got.Done)
	}
	if got.Data["tool_name"] != "Bash" || got.Data["tool_call_id"] != "toolu-1" || got.Data["phase"] != "start" {
		t.Fatalf("stream progress metadata = %#v", got.Data)
	}
}

func TestHandleQueueStatusAppendsReplayableEvent(t *testing.T) {
	stream := &recordingStreamManager{}
	handler := NewAgentStreamHandler(
		context.Background(),
		"session-1",
		"assistant-1",
		"request-1",
		time.Time{},
		&types.Message{ID: "assistant-1", SessionID: "session-1"},
		stream,
		event.NewEventBus(),
	)

	err := handler.handleQueueStatus(context.Background(), event.Event{
		ID:   "queue-1",
		Type: event.EventChatQueueStatus,
		Data: event.ChatQueueStatusData{
			State:          "waiting",
			ModelID:        "model-1",
			ResourcePoolID: "pool-1",
			Position:       3,
			Waiting:        7,
			Active:         2,
			MaxConcurrent:  2,
			MaxWaiting:     20,
			QueuedAtUnix:   123,
		},
	})
	if err != nil {
		t.Fatalf("handleQueueStatus returned error: %v", err)
	}
	if len(stream.events) != 1 {
		t.Fatalf("stream events = %d, want 1", len(stream.events))
	}
	got := stream.events[0]
	if got.Type != types.ResponseTypeQueueStatus || got.Done {
		t.Fatalf("queue stream event = %#v", got)
	}
	if got.Data["position"] != int64(3) || got.Data["resource_pool_id"] != "pool-1" {
		t.Fatalf("queue metadata = %#v", got.Data)
	}
}

func TestUserFacingAgentErrorMessageMapsMaxTurnsAndTimeout(t *testing.T) {
	maxTurns := userFacingAgentErrorMessage(errors.New("Claude result subtype=error_max_turns maxTurns=30 turnCount=31"))
	if maxTurns != "任务过于复杂，请将任务拆分为具体子任务逐个执行，或提高智能体最大迭代次数" {
		t.Fatalf("max turns message = %q", maxTurns)
	}

	timeout := userFacingAgentErrorMessage(errors.New("API request timed out after API_TIMEOUT_MS"))
	if timeout != "任务耗时过长，请将任务拆分为具体子任务逐个执行，或提高智能体LLM调用超时时间" {
		t.Fatalf("timeout message = %q", timeout)
	}

	incompatible := userFacingAgentErrorMessage(errors.New(
		"ResultMessage(result='API Error: Content block is not a text block', model_usage={'secret': 'internal'})",
	))
	if incompatible != "模型服务返回了不兼容的响应格式，请重试或切换模型" {
		t.Fatalf("incompatible response message = %q", incompatible)
	}
	if strings.Contains(incompatible, "model_usage") {
		t.Fatalf("internal model diagnostics leaked to user-facing error: %q", incompatible)
	}

	for _, transportErr := range []string{
		"unexpected EOF",
		"read tcp 10.0.0.1:1234: connection reset by peer",
		"dial tcp 10.0.0.2:8091: connect: connection refused",
		"write: broken pipe",
		"general agent stream ended without result",
		"general agent stream failed: status=504 body=<html>gateway timeout</html>",
	} {
		got := userFacingAgentErrorMessage(errors.New(transportErr))
		if got != "智能体服务连接中断，请稍后重试" {
			t.Fatalf("transport error %q mapped to %q", transportErr, got)
		}
		if strings.Contains(got, "10.0.0.") || strings.Contains(strings.ToLower(got), "status=") {
			t.Fatalf("transport diagnostics leaked to user-facing error: %q", got)
		}
	}
}

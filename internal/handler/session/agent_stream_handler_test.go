package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
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

func TestHandleFinalAnswerStartsNewStreamSegmentWhenProviderReusesIDAfterTool(t *testing.T) {
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

	sharedID := "provider-request-1"
	if err := handler.handleFinalAnswer(context.Background(), event.Event{
		ID: sharedID, Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: "let me search", Done: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := handler.handleToolCall(context.Background(), event.Event{
		ID: sharedID, Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{ToolCallID: "search-1", ToolName: "knowledge_search"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := handler.handleFinalAnswer(context.Background(), event.Event{
		ID: sharedID, Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: "grounded answer", Done: false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := handler.handleFinalAnswer(context.Background(), event.Event{
		ID: sharedID, Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Done: true},
	}); err != nil {
		t.Fatal(err)
	}

	if handler.finalAnswer != "grounded answer" {
		t.Fatalf("finalAnswer = %q, want only the post-tool answer", handler.finalAnswer)
	}
	var answerIDs []string
	for _, streamed := range stream.events {
		if streamed.Type == types.ResponseTypeAnswer {
			answerIDs = append(answerIDs, streamed.ID)
		}
	}
	if len(answerIDs) != 3 {
		t.Fatalf("answer event IDs = %v, want preamble plus final chunks", answerIDs)
	}
	if answerIDs[0] == answerIDs[1] || answerIDs[1] != answerIDs[2] {
		t.Fatalf("answer event IDs = %v, want a new stable ID after the tool call", answerIDs)
	}
}

func TestHandleCompleteUsesCommittedResultAsAuthority(t *testing.T) {
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
			FinalAnswer: "committed corrected answer",
		},
	}); err != nil {
		t.Fatalf("handleComplete returned error: %v", err)
	}

	if msg.Content != "committed corrected answer" {
		t.Fatalf("assistant content = %q, want single final answer", msg.Content)
	}
	if len(stream.events) == 0 {
		t.Fatalf("stream events = 0, want complete event")
	}
	complete := stream.events[len(stream.events)-1]
	if complete.Type != types.ResponseTypeComplete {
		t.Fatalf("last stream event type = %s, want complete", complete.Type)
	}
	if got := complete.Data["final_answer"]; got != "committed corrected answer" {
		t.Fatalf("complete final_answer = %q, want committed answer", got)
	}
	refs, ok := complete.Data["knowledge_references"].([]*types.SearchResult)
	if !ok || len(refs) != 0 {
		t.Fatalf("complete knowledge_references = %#v, want authoritative empty slice", complete.Data["knowledge_references"])
	}
}

func TestHandleCompleteReportsButDoesNotRewriteStreamedCitationProtocol(t *testing.T) {
	stream := &recordingStreamManager{}
	msg := &types.Message{ID: "assistant-1", SessionID: "session-1"}
	handler := NewAgentStreamHandler(
		context.Background(), "session-1", "assistant-1", "request-1", time.Time{},
		msg, stream, event.NewEventBus(),
	)
	ref := &types.SearchResult{
		ID: "chunk-1", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1",
		ChunkType: string(types.ChunkTypeText), Content: "supported claim", EvidenceContent: "supported claim",
		Metadata: map[string]string{"citation_id": "S1", "chunk_id": "chunk-1", "source_type": sourcerefs.SourceTypeKnowledge},
	}
	if err := handler.handleReferences(context.Background(), event.Event{
		Type: event.EventAgentReferences,
		Data: event.AgentReferencesData{References: []*types.SearchResult{ref}},
	}); err != nil {
		t.Fatalf("handleReferences returned error: %v", err)
	}
	raw := `supported claim<src id="S1" /> malformed<doc source_id="S1" />`
	if err := handler.handleFinalAnswer(context.Background(), event.Event{
		ID:   "answer-1",
		Type: event.EventAgentFinalAnswer,
		Data: event.AgentFinalAnswerData{Content: raw, Done: true},
	}); err != nil {
		t.Fatalf("handleFinalAnswer returned error: %v", err)
	}
	if err := handler.handleComplete(context.Background(), event.Event{
		ID:   "complete-1",
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{
			MessageID:                  "assistant-1",
			FinalAnswer:                raw,
			KnowledgeRefs:              []*types.SearchResult{ref},
			KnowledgeRefsAuthoritative: true,
		},
	}); err != nil {
		t.Fatalf("handleComplete returned error: %v", err)
	}

	want := raw
	if msg.Content != want {
		t.Fatalf("assistant content = %q, want %q", msg.Content, want)
	}
	if len(msg.KnowledgeReferences) != 1 || msg.KnowledgeReferences[0].Metadata["citation_id"] != "S1" {
		t.Fatalf("assistant references = %#v, want cited S1 only", msg.KnowledgeReferences)
	}
	complete := stream.events[len(stream.events)-1]
	if got := complete.Data["final_answer"]; got != want {
		t.Fatalf("complete final_answer = %q, want exact streamed answer %q", got, want)
	}
}

func TestHandleCompleteKeepsRetrievedDocumentsSeparateFromCitedFragments(t *testing.T) {
	stream := &recordingStreamManager{}
	msg := &types.Message{ID: "assistant-1", SessionID: "session-1"}
	handler := NewAgentStreamHandler(
		context.Background(), "session-1", "assistant-1", "request-1", time.Time{},
		msg, stream, event.NewEventBus(),
	)
	if err := handler.handleToolCall(context.Background(), event.Event{
		Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{ToolCallID: "search-1", ToolName: "knowledge_search"},
	}); err != nil {
		t.Fatalf("handleToolCall returned error: %v", err)
	}
	refs := []*types.SearchResult{
		{ID: "chunk-1", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText), Content: "claim one", EvidenceContent: "claim one", Metadata: map[string]string{"citation_id": "S1", "chunk_id": "chunk-1", "source_type": sourcerefs.SourceTypeKnowledge}},
		{ID: "chunk-2", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText), Content: "claim two", EvidenceContent: "claim two", Metadata: map[string]string{"citation_id": "S2", "chunk_id": "chunk-2", "source_type": sourcerefs.SourceTypeKnowledge}},
	}
	if err := handler.handleReferences(context.Background(), event.Event{
		Type: event.EventAgentReferences,
		Data: event.AgentReferencesData{References: refs},
	}); err != nil {
		t.Fatalf("handleReferences returned error: %v", err)
	}
	answer := `claim one<src id="S1" />`
	if err := handler.handleComplete(context.Background(), event.Event{
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{
			MessageID:                  "assistant-1",
			FinalAnswer:                answer,
			KnowledgeRefs:              refs[:1],
			KnowledgeRefsAuthoritative: true,
		},
	}); err != nil {
		t.Fatalf("handleComplete returned error: %v", err)
	}

	if len(msg.KnowledgeReferences) != 1 {
		t.Fatalf("cited fragment count = %d, want 1", len(msg.KnowledgeReferences))
	}
	if got := msg.RetrievalStats; !got.Attempted || got.Documents != 1 || got.Total != 1 {
		t.Fatalf("retrieval stats = %+v, want one unique document", got)
	}
	complete := stream.events[len(stream.events)-1]
	stats, ok := complete.Data["retrieval_stats"].(types.RetrievalStats)
	if !ok || stats.Total != 1 {
		t.Fatalf("completion retrieval_stats = %#v", complete.Data["retrieval_stats"])
	}
}

func TestHandleCompletePreservesZeroResultRetrievalAttempt(t *testing.T) {
	stream := &recordingStreamManager{}
	msg := &types.Message{ID: "assistant-1", SessionID: "session-1"}
	handler := NewAgentStreamHandler(
		context.Background(), "session-1", "assistant-1", "request-1", time.Time{},
		msg, stream, event.NewEventBus(),
	)
	_ = handler.handleToolCall(context.Background(), event.Event{
		Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{ToolCallID: "search-1", ToolName: "knowledge_search"},
	})
	if err := handler.handleComplete(context.Background(), event.Event{
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{MessageID: "assistant-1", FinalAnswer: "没有可用资料"},
	}); err != nil {
		t.Fatalf("handleComplete returned error: %v", err)
	}
	if !msg.RetrievalStats.Attempted || msg.RetrievalStats.Total != 0 {
		t.Fatalf("retrieval stats = %+v, want attempted zero-result retrieval", msg.RetrievalStats)
	}
}

func TestHandleCompleteHonorsAuthoritativePlainChatRetrievalStats(t *testing.T) {
	stream := &recordingStreamManager{}
	msg := &types.Message{ID: "assistant-1", SessionID: "session-1"}
	handler := NewAgentStreamHandler(
		context.Background(), "session-1", "assistant-1", "request-1", time.Time{},
		msg, stream, event.NewEventBus(),
	)
	_ = handler.handleToolCall(context.Background(), event.Event{
		Type: event.EventAgentToolCall,
		Data: event.AgentToolCallData{ToolCallID: "fixed-pipeline-search", ToolName: "knowledge_search"},
	})
	if err := handler.handleComplete(context.Background(), event.Event{
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{
			MessageID:                   "assistant-1",
			FinalAnswer:                 "42",
			RetrievalStats:              types.RetrievalStats{},
			RetrievalStatsAuthoritative: true,
		},
	}); err != nil {
		t.Fatalf("handleComplete returned error: %v", err)
	}
	if msg.RetrievalStats.Attempted || msg.RetrievalStats.Total != 0 {
		t.Fatalf("retrieval stats = %+v, want authoritative plain-chat zero state", msg.RetrievalStats)
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

func TestHandleAgentStatusProgressOmitsToolNameFromSSE(t *testing.T) {
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
		ID:   "status-1",
		Type: event.EventAgentProgress,
		Data: event.AgentProgressData{
			Content:    "正在整理最终回答",
			ToolCallID: "status-1",
			Phase:      "start",
			Transient:  true,
			Metadata:   map[string]interface{}{"progress_kind": "assistant_status"},
		},
	}); err != nil {
		t.Fatalf("handleAgentProgress returned error: %v", err)
	}

	if len(stream.events) != 1 {
		t.Fatalf("stream events = %d, want 1", len(stream.events))
	}
	got := stream.events[0]
	if _, exists := got.Data["tool_name"]; exists {
		t.Fatalf("status progress leaked tool_name into SSE: %#v", got.Data)
	}
	if got.Data["progress_kind"] != "assistant_status" || got.Data["tool_call_id"] != "status-1" {
		t.Fatalf("status progress metadata = %#v", got.Data)
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
	if maxTurns != "这次任务未能完成，请缩小问题范围后重试。" {
		t.Fatalf("max turns message = %q", maxTurns)
	}

	timeout := userFacingAgentErrorMessage(errors.New("API request timed out after API_TIMEOUT_MS"))
	if timeout != "这次处理时间较长，未能完成，请稍后重试。" {
		t.Fatalf("timeout message = %q", timeout)
	}

	incompatible := userFacingAgentErrorMessage(errors.New(
		"ResultMessage(result='API Error: Content block is not a text block', model_usage={'secret': 'internal'})",
	))
	if incompatible != "这次未能完成，请稍后重试。" {
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
		if got != "连接暂时中断，请稍后重试。" && got != "这次处理时间较长，未能完成，请稍后重试。" {
			t.Fatalf("transport error %q mapped to %q", transportErr, got)
		}
		if strings.Contains(got, "10.0.0.") || strings.Contains(strings.ToLower(got), "status=") {
			t.Fatalf("transport diagnostics leaked to user-facing error: %q", got)
		}
	}
}

func TestHandleCompleteDeliversArtifactsWithoutToolEvents(t *testing.T) {
	stream := &recordingStreamManager{}
	msg := &types.Message{ID: "assistant-1", SessionID: "session-1"}
	h := NewAgentStreamHandler(context.Background(), "session-1", "assistant-1", "request-1", time.Time{}, msg, stream, event.NewEventBus())
	files := []types.MessageArtifact{{ArtifactID: "file-1", FileName: "result.html", DownloadURL: "/download"}}
	err := h.handleComplete(context.Background(), event.Event{Type: event.EventAgentComplete, Data: event.AgentCompleteData{MessageID: msg.ID, FinalAnswer: "完成", Extra: map[string]interface{}{"artifacts": files, "artifact_notice": "notice"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range stream.events {
		if e.Type == types.ResponseTypeComplete {
			if got, ok := e.Data["artifacts"].([]types.MessageArtifact); !ok || len(got) != 1 || got[0].ArtifactID != "file-1" {
				t.Fatalf("missing artifacts: %#v", e.Data)
			}
			if e.Data["final_answer"] != "完成" {
				t.Fatal("answer was changed")
			}
			return
		}
	}
	t.Fatal("missing completion")
}

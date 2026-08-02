package session

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestQuickAnswerHistoryPersistsRetrievalToolLifecycle(t *testing.T) {
	msg := &types.Message{}
	recordQuickAnswerToolCall(msg, event.AgentToolCallData{
		ToolCallID: "search-1",
		ToolName:   "knowledge_search",
		Arguments:  map[string]any{"query": "投资管理办法"},
	})
	recordQuickAnswerToolResult(msg, event.AgentToolResultData{
		ToolCallID: "search-1",
		ToolName:   "knowledge_search",
		Output:     "检索到 3 条相关内容",
		Success:    true,
		Duration:   18,
		Data:       map[string]interface{}{"count": 3},
	})
	appendQuickAnswerReasoning(msg, "需要依据检索结果回答。")

	if len(msg.AgentSteps) != 1 {
		t.Fatalf("agent steps = %d, want 1", len(msg.AgentSteps))
	}
	step := msg.AgentSteps[0]
	if step.ReasoningContent != "需要依据检索结果回答。" {
		t.Fatalf("reasoning content = %q", step.ReasoningContent)
	}
	if len(step.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(step.ToolCalls))
	}
	call := step.ToolCalls[0]
	if call.Name != "knowledge_search" || call.Args["query"] != "投资管理办法" {
		t.Fatalf("unexpected persisted tool call: %#v", call)
	}
	if call.Result == nil || !call.Result.Success || call.Result.Data["count"] != 3 {
		t.Fatalf("unexpected persisted tool result: %#v", call.Result)
	}
	if call.Duration != 18 {
		t.Fatalf("duration = %d, want 18", call.Duration)
	}
}

func TestQuickAnswerHistoryRecordsResultEvenWhenStartWasMissed(t *testing.T) {
	msg := &types.Message{}
	recordQuickAnswerToolResult(msg, event.AgentToolResultData{
		ToolCallID: "understand-1",
		ToolName:   "query_understand",
		Output:     "已完成问题理解",
		Success:    true,
	})

	if len(msg.AgentSteps) != 1 || len(msg.AgentSteps[0].ToolCalls) != 1 {
		t.Fatalf("unexpected steps: %#v", msg.AgentSteps)
	}
	call := msg.AgentSteps[0].ToolCalls[0]
	if call.ID != "understand-1" || call.Name != "query_understand" || call.Result == nil {
		t.Fatalf("unexpected recovered tool call: %#v", call)
	}
}

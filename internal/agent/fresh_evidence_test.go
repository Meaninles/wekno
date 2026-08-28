package agent

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestShouldRetryForFreshEvidence(t *testing.T) {
	retrievalTools := []chat.Tool{{
		Type:     "function",
		Function: chat.FunctionDef{Name: "grep_chunks"},
	}}
	answer := &types.ChatResponse{Content: `制度规定如下<src id="S1" />`}
	query := "请依据已选制度回答并就近引用。"

	if !shouldRetryForFreshEvidence(query, retrievalTools, answer, nil, 0) {
		t.Fatal("explicit fresh-evidence request without retrieval should retry")
	}
	if shouldRetryForFreshEvidence(query, retrievalTools, answer, nil, 1) {
		t.Fatal("fresh-evidence retry must be bounded")
	}
	if shouldRetryForFreshEvidence("直接回答即可", retrievalTools, answer, nil, 0) {
		t.Fatal("ordinary request must not gain an evidence retry")
	}
	if shouldRetryForFreshEvidence(query, nil, answer, nil, 0) {
		t.Fatal("request without an available retrieval tool cannot retry")
	}
	withToolCall := &types.ChatResponse{
		Content:   "先检索",
		ToolCalls: []types.LLMToolCall{{}},
	}
	if shouldRetryForFreshEvidence(query, retrievalTools, withToolCall, nil, 0) {
		t.Fatal("an in-flight tool call must not be retried")
	}
}

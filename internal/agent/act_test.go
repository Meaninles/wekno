package agent

import (
	"strings"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
)

func TestTargetedEvidenceRetrievalRedirectsOnlyWholeDocumentMultiTopicReads(t *testing.T) {
	query := "[WEKNORA_REQUIRED_EVIDENCE_TOPICS][\"公开采购\",\"询比\",\"竞价\",\"竞争谈判\"]"
	redirect := targetedEvidenceRetrievalRedirect(
		agenttools.ToolListKnowledgeChunks,
		map[string]interface{}{"knowledge_id": "document-1", "limit": float64(100)},
		query,
	)
	if redirect == nil || redirect.Success || !strings.Contains(redirect.Error, "one query for each topic") {
		t.Fatalf("whole-document read was not redirected: %#v", redirect)
	}
	if !strings.Contains(redirect.Error, "公开采购") || !strings.Contains(redirect.Error, "竞争谈判") {
		t.Fatalf("redirect omitted named topics: %s", redirect.Error)
	}

	if got := targetedEvidenceRetrievalRedirect(
		agenttools.ToolListKnowledgeChunks,
		map[string]interface{}{"chunk_id": "chunk-1"},
		query,
	); got != nil {
		t.Fatalf("exact chunk read was redirected: %#v", got)
	}
	if got := targetedEvidenceRetrievalRedirect(
		agenttools.ToolListKnowledgeChunks,
		map[string]interface{}{"knowledge_id": "document-1"},
		"只回答一个定义并引用。",
	); got != nil {
		t.Fatalf("ordinary whole-document read was redirected: %#v", got)
	}
}

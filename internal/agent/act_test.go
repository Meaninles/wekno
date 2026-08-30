package agent

import (
	"strings"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
)

func TestTargetedEvidenceRetrievalRedirectsFocusedWholeDocumentReads(t *testing.T) {
	query := "[WEKNORA_REQUIRED_EVIDENCE_TOPICS][\"公开采购\",\"询比\",\"竞价\",\"竞争谈判\"]\n" +
		"[WEKNORA_REQUIRED_EVIDENCE_SEARCHES][\"公开采购 适用条件\",\"询比 适用条件\",\"竞价 适用条件\",\"竞争谈判 适用条件\"]"
	redirect := agenttools.TargetedEvidenceRetrievalRedirect(
		agenttools.ToolListKnowledgeChunks,
		map[string]interface{}{"knowledge_id": "document-1", "limit": float64(100)},
		query,
	)
	if redirect == nil || redirect.Success || !strings.Contains(redirect.Error, "knowledge_search first") {
		t.Fatalf("whole-document read was not redirected: %#v", redirect)
	}
	if !strings.Contains(redirect.Error, "公开采购") || !strings.Contains(redirect.Error, "竞争谈判") {
		t.Fatalf("redirect omitted named topics: %s", redirect.Error)
	}

	if got := agenttools.TargetedEvidenceRetrievalRedirect(
		agenttools.ToolListKnowledgeChunks,
		map[string]interface{}{"chunk_id": "chunk-1"},
		query,
	); got != nil {
		t.Fatalf("exact chunk read was redirected: %#v", got)
	}
	if got := agenttools.TargetedEvidenceRetrievalRedirect(
		agenttools.ToolListKnowledgeChunks,
		map[string]interface{}{"knowledge_id": "document-1"},
		"只回答一个定义并引用。",
	); got != nil {
		t.Fatalf("ordinary whole-document read was redirected: %#v", got)
	}

	single := conversationmemory.AppendCurrentTurnDirective(
		"解释Skill的渐进式披露机制，并给出引用。",
		"解释Skill的渐进式披露机制，并给出引用。",
	)
	if got := agenttools.TargetedEvidenceRetrievalRedirect(
		agenttools.ToolListKnowledgeChunks,
		map[string]interface{}{"knowledge_id": "document-1"},
		single,
	); got == nil || !strings.Contains(got.Error, "渐进式披露") {
		t.Fatalf("single-topic whole-document read was not redirected: %#v", got)
	}

	exhaustive := single + "\n用户要求通读全文。"
	if got := agenttools.TargetedEvidenceRetrievalRedirect(
		agenttools.ToolListKnowledgeChunks,
		map[string]interface{}{"knowledge_id": "document-1"},
		exhaustive,
	); got != nil {
		t.Fatalf("genuine exhaustive review was redirected: %#v", got)
	}
}

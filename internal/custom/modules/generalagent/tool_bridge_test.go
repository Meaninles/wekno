package generalagent

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestActiveRunCitationContractActivatesAfterCitableEvidence(t *testing.T) {
	run := &activeRun{
		ctx:       context.Background(),
		eventBus:  event.NewEventBus(),
		sessionID: "session-1",
		requestID: "request-1",
	}
	result := &types.ToolResult{
		Success: true,
		Output:  `<knowledge_chunks><chunk chunk_id="chunk-1"><content>证据</content></chunk></knowledge_chunks>`,
		Data: map[string]interface{}{
			"display_type":    "knowledge_chunks_list",
			"knowledge_id":    "doc-1",
			"knowledge_title": "制度.docx",
			"chunks": []map[string]interface{}{{
				"seq": 1, "chunk_id": "chunk-1", "knowledge_id": "doc-1",
				"knowledge_base_id": "kb-1", "content": "证据",
			}},
		},
	}

	if sources := run.registerSourceReferences("list_knowledge_chunks", result); len(sources) != 1 {
		t.Fatalf("source count = %d, want 1", len(sources))
	}
	if !run.hasCitableEvidence() {
		t.Fatal("citable evidence did not activate the shared terminal contract")
	}
	if instruction := sourcerefs.TerminalCitationInstruction(); instruction == "" {
		t.Fatal("shared terminal citation instruction is empty")
	}
}

func TestActiveRunCitationContractIgnoresStructuredDataSources(t *testing.T) {
	run := &activeRun{
		ctx:       context.Background(),
		eventBus:  event.NewEventBus(),
		sessionID: "session-1",
		requestID: "request-1",
	}
	result := &types.ToolResult{
		Success: true,
		Output:  "query result",
		Data: map[string]interface{}{
			"display_type": "structured_analysis_result",
			"rows":         []interface{}{map[string]interface{}{"count": 1}},
			"source":       map[string]interface{}{"source_ids": []string{"db-1"}},
		},
	}

	if sources := run.registerSourceReferences("db_query", result); len(sources) != 0 {
		t.Fatalf("structured data should not create inline citation sources: %#v", sources)
	}
	if run.hasCitableEvidence() {
		t.Fatal("structured data activated the document/Wiki/web citation contract")
	}
}

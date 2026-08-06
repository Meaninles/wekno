package agent

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestPrepareFinalAnswerCitationContextUsesDocumentFragments(t *testing.T) {
	state := &types.AgentState{
		RoundSteps: []types.AgentStep{
			{
				ToolCalls: []types.ToolCall{
					{
						Name: "knowledge_search",
						Result: &types.ToolResult{
							Success: true,
							Data: map[string]interface{}{
								"display_type": "search_results",
								"results": []map[string]interface{}{
									{
										"chunk_id":          "chunk-1",
										"knowledge_id":      "doc-1",
										"knowledge_base_id": "kb-1",
										"knowledge_title":   "智能体开发指南.md",
										"chunk_index":       0,
										"content":           "第一段文档片段内容。",
									},
									{
										"chunk_id":          "chunk-2",
										"knowledge_id":      "doc-1",
										"knowledge_base_id": "kb-1",
										"knowledge_title":   "智能体开发指南.md",
										"chunk_index":       1,
										"content":           "第二段文档片段内容。",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	context, refs := prepareFinalAnswerCitationContext(state)

	if len(refs) != 2 {
		t.Fatalf("refs len = %d, want 2", len(refs))
	}
	if len(state.KnowledgeRefs) != 2 {
		t.Fatalf("state knowledge refs len = %d, want 2", len(state.KnowledgeRefs))
	}
	if got := refs[0].Metadata[sourcerefs.MetadataCitationID]; got != "S1" {
		t.Fatalf("first citation id = %q, want S1", got)
	}
	if got := refs[1].Metadata[sourcerefs.MetadataCitationID]; got != "S2" {
		t.Fatalf("second citation id = %q, want S2", got)
	}
	for _, expected := range []string{
		`cite_exactly=<src id="S1" />`,
		`cite_exactly=<src id="S2" />`,
	} {
		if !strings.Contains(context, expected) {
			t.Fatalf("citation context missing %q: %s", expected, context)
		}
	}
	for _, duplicatedEvidence := range []string{"[CITATION_EVIDENCE]", "第一段文档片段内容。", "第二段文档片段内容。"} {
		if strings.Contains(context, duplicatedEvidence) {
			t.Fatalf("citation context should not duplicate tool evidence %q: %s", duplicatedEvidence, context)
		}
	}
	for _, forbidden := range []string{`<source `, `<context `, `source_id=`, `chunk_id=`} {
		if strings.Contains(context, forbidden) {
			t.Fatalf("citation context should not prime alternate citation syntax %q: %s", forbidden, context)
		}
	}
}

package sourcerefs

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestExtractFromToolResultRequiresClaimBearingEvidence(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		result   *types.ToolResult
		wantType string
		want     int
	}{
		{
			name:     "knowledge search fragment",
			toolName: "knowledge_search",
			result: &types.ToolResult{Success: true, Data: map[string]interface{}{
				"display_type": "search_results",
				"results": []interface{}{map[string]interface{}{
					"chunk_id": "chunk-1", "knowledge_id": "doc-1", "knowledge_base_id": "kb-1",
					"knowledge_base_name": "公司制度1", "knowledge_title": "制度", "content": "审批上限为十万元。",
					"result_index": 3, "start_at": 12, "end_at": 24,
				}},
			}},
			wantType: SourceTypeKnowledge,
			want:     1,
		},
		{
			name:     "grep catalog hit is not evidence",
			toolName: "grep_chunks",
			result: &types.ToolResult{Success: true, Data: map[string]interface{}{
				"display_type":      "grep_results",
				"knowledge_results": []interface{}{map[string]interface{}{"knowledge_id": "doc-1", "title": "制度"}},
			}},
			want: 0,
		},
		{
			name:     "wiki search summary is not evidence",
			toolName: "wiki_search",
			result:   &types.ToolResult{Success: true, Output: `<page><link>[[ops/guide|运维指南]]</link><summary>摘要</summary></page>`, Data: map[string]interface{}{"found_kbs": map[string]interface{}{"ops/guide": []interface{}{"kb-1"}}}},
			want:     0,
		},
		{
			name:     "fully read wiki page",
			toolName: "wiki_read_page",
			result:   &types.ToolResult{Success: true, Output: `<wiki_page><metadata><knowledge_base_id>kb-1</knowledge_base_id><link>[[ops/guide|运维指南]]</link></metadata><summary>摘要</summary><content>完整正文</content></wiki_page>`, Data: map[string]interface{}{}},
			wantType: SourceTypeWiki,
			want:     1,
		},
		{
			name:     "wiki source document returns citable fragments",
			toolName: "wiki_read_source_doc",
			result: &types.ToolResult{Success: true, Data: map[string]interface{}{
				"display_type": "knowledge_chunks_list",
				"chunks": []interface{}{map[string]interface{}{
					"chunk_id": "chunk-source-1", "knowledge_id": "doc-source-1",
					"knowledge_base_id": "kb-1", "knowledge_title": "采购管理办法",
					"chunk_index": 4, "content": "采购金额达到三十万元时应履行对应审批流程。",
				}},
			}},
			wantType: SourceTypeKnowledge,
			want:     1,
		},
		{
			name:     "partially successful web fetch keeps raw fetched evidence",
			toolName: "web_fetch",
			result: &types.ToolResult{Success: false, Data: map[string]interface{}{
				"display_type": "web_fetch_results",
				"results": []interface{}{map[string]interface{}{
					"url": "https://example.com/CaseSensitive", "raw_content": "网页原文证据", "error": "summary unavailable",
				}},
			}},
			wantType: SourceTypeWeb,
			want:     1,
		},
		{
			name:     "database catalog is not evidence",
			toolName: "db_schema",
			result:   &types.ToolResult{Success: true, Data: map[string]interface{}{"display_type": "db_schema", "tables": []interface{}{"orders"}}},
			want:     0,
		},
		{
			name:     "structured query rows are immutable evidence",
			toolName: "db_query",
			result: &types.ToolResult{Success: true, Output: "共 2 行", Data: map[string]interface{}{
				"display_type": "structured_analysis_result", "query": "select region, total from sales",
				"analysis_type": "database",
				"source": map[string]interface{}{
					"type": "database", "source_count": 1, "source_names": []interface{}{"销售库"},
				},
				"columns": []interface{}{"region", "total"},
				"rows":    []interface{}{[]interface{}{"华东", 12}, []interface{}{"华南", 8}},
			}},
			wantType: SourceTypeData,
			want:     1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refs := ExtractFromToolResult(tc.toolName, tc.result)
			if len(refs) != tc.want {
				t.Fatalf("got %d refs, want %d: %#v", len(refs), tc.want, refs)
			}
			if tc.want > 0 && SourceTypeFromRef(refs[0]) != tc.wantType {
				t.Fatalf("source type = %q, want %q", SourceTypeFromRef(refs[0]), tc.wantType)
			}
			if tc.name == "knowledge search fragment" && refs[0].Metadata["knowledge_base_name"] != "公司制度1" {
				t.Fatalf("knowledge-base provenance was lost: %#v", refs[0])
			}
			if tc.name == "knowledge search fragment" &&
				(refs[0].Metadata["tool_result_position"] != "3" || refs[0].StartAt != 12 || refs[0].EndAt != 24) {
				t.Fatalf("fragment position or coordinates were lost: %#v", refs[0])
			}
		})
	}
}

func TestRegistryRefreshesEvidenceSnapshotWithoutChangingHandle(t *testing.T) {
	registry := NewRegistry()
	first := &types.SearchResult{
		ID: "https://Example.com/CaseSensitive", Content: "搜索摘要", KnowledgeTitle: "原始网页标题",
		ChunkType: "web_search", Metadata: map[string]string{"source_type": SourceTypeWeb, "url": "https://Example.com/CaseSensitive"},
	}
	registry.Register([]*types.SearchResult{first})
	firstHash := first.Metadata[MetadataEvidenceHash]
	second := &types.SearchResult{
		ID: "https://example.com/CaseSensitive", Content: "完整网页正文", KnowledgeTitle: "example.com",
		ChunkType: "web_search", Metadata: map[string]string{"source_type": SourceTypeWeb, "url": "https://example.com/CaseSensitive"},
	}
	sources := registry.Register([]*types.SearchResult{second})
	snapshots := registry.SnapshotReferences()
	if CitationID(first) != "S1" || CitationID(second) != "S1" || len(snapshots) != 1 {
		t.Fatalf("same normalized URL did not retain one handle: first=%q second=%q snapshots=%d", CitationID(first), CitationID(second), len(snapshots))
	}
	if snapshots[0].Content != "完整网页正文" || snapshots[0].Metadata[MetadataEvidenceHash] == firstHash {
		t.Fatalf("latest evidence snapshot was not retained: %#v", snapshots[0])
	}
	if len(sources) != 1 || sources[0].Title != "原始网页标题" {
		t.Fatalf("useful source title was downgraded: %#v", sources)
	}
	if !strings.Contains(sources[0].URL, "/CaseSensitive") {
		t.Fatalf("URL path case was lost: %q", sources[0].URL)
	}
}

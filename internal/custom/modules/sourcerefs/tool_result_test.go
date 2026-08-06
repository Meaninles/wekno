package sourcerefs

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestRegisterToolResultExposesStableFragmentHandleAndCoordinates(t *testing.T) {
	result := &types.ToolResult{
		Success: true,
		Output:  `<knowledge_chunks><chunk chunk_id="chunk-531"><content>5.3.1要求包含项目名称和投资金额。</content></chunk></knowledge_chunks>`,
		Data: map[string]interface{}{
			"display_type":    "knowledge_chunks_list",
			"knowledge_id":    "doc-1",
			"knowledge_title": "投资管理办法.docx",
			"chunks": []map[string]interface{}{
				{
					"seq":               1,
					"chunk_id":          "chunk-531",
					"knowledge_id":      "doc-1",
					"knowledge_base_id": "kb-company",
					"chunk_index":       18,
					"start_at":          220,
					"end_at":            260,
					"content":           "5.3.1要求包含项目名称和投资金额。",
					"source_locator": map[string]interface{}{
						"kind":                "page_range",
						"page_start":          8,
						"physical_part_index": 2,
					},
				},
			},
		},
	}

	registry := NewRegistry()
	refs, sources := RegisterToolResult(registry, "list_knowledge_chunks", result)
	AttachToolResultSources(result, sources)
	if len(refs) != 1 || len(sources) != 1 {
		t.Fatalf("refs=%d sources=%d, want one fragment", len(refs), len(sources))
	}
	if got := CitationID(refs[0]); got != "S1" {
		t.Fatalf("citation id = %q, want S1", got)
	}
	if refs[0].KnowledgeBaseID != "kb-company" || refs[0].StartAt != 220 || refs[0].EndAt != 260 {
		t.Fatalf("fragment provenance was lost: %#v", refs[0])
	}
	if !strings.Contains(string(refs[0].SourceLocator), `"physical_part_index":2`) {
		t.Fatalf("complete source locator was not preserved: %s", refs[0].SourceLocator)
	}
	if sources[0].ResultPosition != 1 || sources[0].CiteExactly != `<src id="S1" />` {
		t.Fatalf("source descriptor is not directly copyable: %#v", sources[0])
	}
	structured, ok := result.Data["source_references"].([]*CitationSource)
	if !ok || len(structured) != 1 || structured[0].ID != "S1" {
		t.Fatalf("structured source_references missing: %#v", result.Data["source_references"])
	}
	result.Output = AppendCitationCatalog(result.Output, refs)
	for _, expected := range []string{
		`chunk_id="chunk-531">`,
		`citation_handle_for_this_evidence: <src id="S1" />`,
	} {
		if !strings.Contains(result.Output, expected) {
			t.Fatalf("model-visible handle missing %q: %s", expected, result.Output)
		}
	}
	if strings.Contains(result.Output, "tool_result_position") || strings.Contains(result.Output, "[AVAILABLE_CITATIONS]") {
		t.Fatalf("resolved evidence should be annotated inline without an ordinal/catalog ambiguity: %s", result.Output)
	}
	contentAt := strings.Index(result.Output, "5.3.1要求")
	handleAt := strings.Index(result.Output, `citation_handle_for_this_evidence: <src id="S1" />`)
	closeAt := strings.Index(result.Output, "</chunk>")
	if contentAt < 0 || handleAt < contentAt || closeAt < handleAt {
		t.Fatalf("handle must follow its evidence and remain inside the chunk: %s", result.Output)
	}
	if strings.Contains(result.Output, "[CITATION_USE]") {
		t.Fatalf("terminal contract must be added once at generation time, not per tool result: %s", result.Output)
	}

	second := &types.ToolResult{Success: true, Data: result.Data}
	secondRefs, secondSources := RegisterToolResult(registry, "list_knowledge_chunks", second)
	if len(secondRefs) != 1 || len(secondSources) != 1 || CitationID(secondRefs[0]) != "S1" {
		t.Fatalf("same fragment did not retain S1: refs=%#v sources=%#v", secondRefs, secondSources)
	}
}

func TestRegisterToolResultDoesNotCreateStructuredDataCitation(t *testing.T) {
	registry := NewRegistry()
	result := &types.ToolResult{
		Success: true,
		Output:  "query result",
		Data: map[string]interface{}{
			"display_type": "structured_analysis_result",
			"query":        "select count(*) from orders",
			"rows":         []interface{}{map[string]interface{}{"count": 1}},
			"source": map[string]interface{}{
				"source_ids": []string{"db-1"},
			},
		},
	}
	refs, sources := RegisterToolResult(registry, "db_query", result)
	if len(refs) != 0 || len(sources) != 0 || len(registry.SnapshotReferences()) != 0 {
		t.Fatalf("structured data escaped the three-type citation boundary: refs=%#v sources=%#v", refs, sources)
	}
}

func TestAppendCitationCatalogUsesRunScopedIDsInsteadOfToolOrdinals(t *testing.T) {
	registry := NewRegistry()
	for i := 1; i <= 10; i++ {
		refs := []*types.SearchResult{{
			ID:              "search-chunk-" + string(rune('a'+i-1)),
			Content:         "search evidence",
			KnowledgeID:     "search-doc",
			KnowledgeBaseID: "kb-company",
			KnowledgeTitle:  "搜索结果.docx",
			ChunkType:       "text",
			Metadata:        map[string]string{"source_type": SourceTypeKnowledge},
		}}
		registry.Register(refs)
	}

	result := &types.ToolResult{
		Success: true,
		Output: `<knowledge_chunks>
<chunk chunk_id="target-528"><content>5.2.8股东大会条件。</content></chunk>
<chunk chunk_id="target-531"><content>5.3.1明确内容。</content></chunk>
</knowledge_chunks>`,
		Data: map[string]interface{}{
			"display_type":    "knowledge_chunks_list",
			"knowledge_id":    "investment-doc",
			"knowledge_title": "投资管理办法.docx",
			"chunks": []map[string]interface{}{
				{"seq": 6, "chunk_id": "target-528", "knowledge_id": "investment-doc", "knowledge_base_id": "kb-company", "content": "5.2.8股东大会条件。"},
				{"seq": 7, "chunk_id": "target-531", "knowledge_id": "investment-doc", "knowledge_base_id": "kb-company", "content": "5.3.1明确内容。"},
			},
		},
	}
	refs, _ := RegisterToolResult(registry, "list_knowledge_chunks", result)
	if len(refs) != 2 || CitationID(refs[0]) != "S11" || CitationID(refs[1]) != "S12" {
		t.Fatalf("unexpected run-scoped IDs: %#v", refs)
	}
	output := AppendCitationCatalog(result.Output, refs)
	for _, expected := range []string{
		`chunk_id="target-528"><content>`,
		`citation_handle_for_this_evidence: <src id="S11" />`,
		`citation_handle_for_this_evidence: <src id="S12" />`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("missing %q from annotated output: %s", expected, output)
		}
	}
	if strings.Contains(output, "tool_result_position=6") || strings.Contains(output, "tool_result_position=7") {
		t.Fatalf("tool ordinals must not be exposed beside citation IDs: %s", output)
	}
}

func TestAttachCitationHandlesToWikiAndWebEvidence(t *testing.T) {
	wiki := &types.SearchResult{
		ID:              "wiki:kb-1:concept/lightning",
		Content:         "防雷装置内容",
		KnowledgeBaseID: "kb-1",
		KnowledgeTitle:  "防雷装置",
		ChunkType:       "wiki_page",
		Metadata:        map[string]string{"source_type": SourceTypeWiki, "slug": "concept/lightning"},
	}
	web := &types.SearchResult{
		ID:             "https://example.com/report",
		Content:        "网页内容",
		KnowledgeTitle: "报告",
		ChunkType:      "web_search",
		Metadata:       map[string]string{"source_type": SourceTypeWeb, "url": "https://example.com/report"},
	}
	AssignCitationIDs([]*types.SearchResult{wiki, web})

	output := `<wiki_page>
<link>[[concept/lightning|防雷装置]]</link>
<content>防雷装置内容</content>
</wiki_page>
URL: https://example.com/report
Content: 网页内容`
	annotated, unresolved := AttachCitationHandlesToEvidence(output, []*types.SearchResult{wiki, web})
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved refs: %#v", unresolved)
	}
	for _, expected := range []string{
		`citation_handle_for_this_evidence: <src id="S1" />`,
		`citation_handle_for_this_evidence: <src id="S2" />`,
	} {
		if !strings.Contains(annotated, expected) {
			t.Fatalf("missing %q from annotated output: %s", expected, annotated)
		}
	}
}

func TestAttachCitationHandlePrefersExactFragmentBoundary(t *testing.T) {
	ref := &types.SearchResult{
		ID: "child-2", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1",
		Content: "第二段", EvidenceContent: "第二段", ChunkType: string(types.ChunkTypeText),
		Metadata: map[string]string{"source_type": SourceTypeKnowledge},
	}
	AssignCitationIDs([]*types.SearchResult{ref})
	output := `<chunk chunk_id="child-2">
<content>[EXACT_FRAGMENT chunk_id="child-1"]
第一段
[/EXACT_FRAGMENT]
[EXACT_FRAGMENT chunk_id="child-2"]
第二段
[/EXACT_FRAGMENT]</content>
</chunk>`
	annotated, unresolved := AttachCitationHandlesToEvidence(output, []*types.SearchResult{ref})
	if len(unresolved) != 0 {
		t.Fatalf("exact fragment handle was unresolved: %#v", unresolved)
	}
	marker := `citation_handle_for_this_evidence: <src id="S1" />`
	exactAt := strings.Index(annotated, `[EXACT_FRAGMENT chunk_id="child-2"]`)
	markerAt := strings.Index(annotated, marker)
	if exactAt < 0 || markerAt < exactAt {
		t.Fatalf("handle attached to aggregate wrapper instead of exact child: %s", annotated)
	}
}

func TestAttachCitationHandlesToWikiUsesOwningPageNotEarlierCrossLink(t *testing.T) {
	finance := &types.SearchResult{
		ID:              "wiki:kb-1:entity/finance-department",
		Content:         "财务部职责",
		KnowledgeBaseID: "kb-1",
		KnowledgeTitle:  "财务部",
		ChunkType:       "wiki_page",
		Metadata: map[string]string{
			"source_type": SourceTypeWiki,
			"slug":        "entity/finance-department",
		},
	}
	discipline := &types.SearchResult{
		ID:              "wiki:kb-1:entity/gongsi-jiwei",
		Content:         "公司纪委职责",
		KnowledgeBaseID: "kb-1",
		KnowledgeTitle:  "公司纪委",
		ChunkType:       "wiki_page",
		Metadata: map[string]string{
			"source_type": SourceTypeWiki,
			"slug":        "entity/gongsi-jiwei",
		},
	}
	AssignCitationIDs([]*types.SearchResult{finance, discipline})

	output := `<wiki_page>
<metadata><link>[[entity/finance-department|财务部]]</link></metadata>
<relationships><links_to>[[entity/gongsi-jiwei|公司纪委]]</links_to></relationships>
<content>财务部负责资金监督。</content>
</wiki_page>
<wiki_page>
<metadata><link>[[entity/gongsi-jiwei|公司纪委]]</link></metadata>
<content>公司纪委负责处理违规行为。</content>
</wiki_page>`
	annotated, unresolved := AttachCitationHandlesToEvidence(
		output, []*types.SearchResult{finance, discipline},
	)
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved refs: %#v", unresolved)
	}
	pages := wikiPageRE.FindAllString(annotated, -1)
	if len(pages) != 2 {
		t.Fatalf("wiki page count = %d, want 2: %s", len(pages), annotated)
	}
	if !strings.Contains(pages[0], `<src id="S1" />`) || strings.Contains(pages[0], `<src id="S2" />`) {
		t.Fatalf("finance page received the wrong handle: %s", pages[0])
	}
	if !strings.Contains(pages[1], `<src id="S2" />`) || strings.Contains(pages[1], `<src id="S1" />`) {
		t.Fatalf("discipline page received the wrong handle: %s", pages[1])
	}
}

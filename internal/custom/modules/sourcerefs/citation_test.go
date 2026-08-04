package sourcerefs

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestAssignCitationIDsSeparatesKnowledgeChunksWithinDocument(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID:              "chunk-1",
			Content:         "第一段证据",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkIndex:      0,
			ChunkType:       "text",
			SourceLocator:   types.JSON(`{"kind":"sheet_range","sheet":"资产目录","row_start":90001,"row_end":91000}`),
			Metadata:        map[string]string{"knowledge_base_name": "公司制度1", "tool_result_position": "1"},
		},
		{
			ID:              "chunk-2",
			Content:         "第二段证据",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkIndex:      1,
			ChunkType:       "text",
		},
	}

	sources := AssignCitationIDs(refs)
	if len(sources) != 2 {
		t.Fatalf("sources len = %d, want 2", len(sources))
	}
	if got := refs[0].Metadata[MetadataCitationID]; got != "S1" {
		t.Fatalf("first citation id = %q, want S1", got)
	}
	if got := refs[1].Metadata[MetadataCitationID]; got != "S2" {
		t.Fatalf("second citation id = %q, want S2", got)
	}
	if got := sources[0].KnowledgeID; got != "doc-1" {
		t.Fatalf("source knowledge id = %q, want doc-1", got)
	}
	if got := sources[0].ChunkID; got != "chunk-1" {
		t.Fatalf("first source chunk id = %q, want chunk-1", got)
	}
	if got := sources[0].Granularity; got != "document_fragment" {
		t.Fatalf("first source granularity = %q, want document_fragment", got)
	}
	if got := sources[0].KnowledgeBaseName; got != "公司制度1" {
		t.Fatalf("source knowledge base name = %q, want 公司制度1", got)
	}
	if got := refs[1].Metadata[MetadataChunkID]; got != "chunk-2" {
		t.Fatalf("second metadata chunk id = %q, want chunk-2", got)
	}
	if got := string(sources[0].SourceLocator); !strings.Contains(got, `"row_start":90001`) {
		t.Fatalf("logical source locator was lost: %q", got)
	}
	if got := refs[0].Metadata["source_locator"]; !strings.Contains(got, `"sheet":"资产目录"`) {
		t.Fatalf("citation metadata source locator was lost: %q", got)
	}
	if catalog := RenderCitationCatalog(refs); !strings.Contains(catalog, `cite_exactly=<src id="S1" />`) ||
		!strings.Contains(catalog, `type=document_fragment`) || !strings.Contains(catalog, `collection="公司制度1"`) ||
		strings.Contains(catalog, `tool_result_position`) || strings.Contains(catalog, `chunk_id=`) ||
		strings.Contains(catalog, `<source `) {
		t.Fatalf("catalog should expose only the exact positive citation shape and compact evidence metadata, got %s", catalog)
	}
	if block := RenderEvidenceBlock(refs[0], "claim-bearing content", map[string]string{"match": "exact"}); !strings.Contains(block, `[EVIDENCE id=S1 type=document_fragment`) ||
		!strings.Contains(block, `collection="公司制度1"`) || !strings.Contains(block, `match=exact`) ||
		!strings.Contains(block, `citation_handle_for_this_evidence: <src id="S1" />`) ||
		strings.Index(block, `citation_handle_for_this_evidence`) < strings.Index(block, "claim-bearing content") ||
		strings.Contains(block, `chunk_id`) || strings.Contains(block, `<document`) {
		t.Fatalf("evidence block should use the citation id without alternate XML citation shapes, got %s", block)
	}
	if got := sources[0].CiteExactly; got != `<src id="S1" />` {
		t.Fatalf("cite_exactly = %q, want canonical source tag", got)
	}
}

func TestPlaceTerminalCitationInstructionIsEvidenceAwareIdempotentAndLast(t *testing.T) {
	refs := []*types.SearchResult{{
		ID:              "chunk-1",
		KnowledgeID:     "doc-1",
		KnowledgeBaseID: "kb-1",
		Content:         "claim",
		ChunkType:       string(types.ChunkTypeText),
		Metadata:        map[string]string{},
	}}
	AssignCitationIDs(refs)

	content := citationUseInstruction + "\n\n[EVIDENCE]\nclaim\n[/EVIDENCE]"
	got := PlaceTerminalCitationInstruction(content, refs)
	if strings.Count(got, "[CITATION_USE]") != 1 {
		t.Fatalf("terminal instruction should occur once: %s", got)
	}
	if !strings.HasSuffix(got, citationUseInstruction) {
		t.Fatalf("terminal instruction should be last: %s", got)
	}
	if twice := PlaceTerminalCitationInstruction(got, refs); twice != got {
		t.Fatalf("terminal instruction placement is not idempotent:\nfirst=%s\nsecond=%s", got, twice)
	}
	if !strings.Contains(got, "The handle must appear in the final user-visible answer") {
		t.Fatalf("terminal instruction does not require a visible final citation: %s", got)
	}

	dataRef := &types.SearchResult{Metadata: map[string]string{"source_type": SourceTypeData}}
	if noEvidence := PlaceTerminalCitationInstruction("plain answer", []*types.SearchResult{dataRef}); noEvidence != "plain answer" {
		t.Fatalf("data sources must not activate the citation contract: %s", noEvidence)
	}
}

func TestAssignCitationIDsRejectsDocumentLevelKnowledgeWithoutFragment(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID:              "doc-1",
			Content:         "只有文档级内容",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkType:       "text",
		},
		{
			ID:              "doc-1",
			Content:         "只有文档级内容",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkType:       "text",
		},
	}

	sources := AssignCitationIDs(refs)
	if len(sources) != 0 || CitationID(refs[0]) != "" || CitationID(refs[1]) != "" {
		t.Fatalf("document-level knowledge escaped the fragment citation boundary: %#v", sources)
	}
}

func TestAssignCitationIDsUsesDistinctWikiSlug(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID:              "wiki:kb-1:ops/bastion",
			Content:         "堡垒机页面正文",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkType:       "wiki_page",
			Metadata: map[string]string{
				"source_type": SourceTypeWiki,
				"slug":        "ops/bastion",
			},
		},
	}

	sources := AssignCitationIDs(refs)
	if len(sources) != 1 {
		t.Fatalf("sources len = %d, want 1", len(sources))
	}
	if sources[0].Slug != "ops/bastion" {
		t.Fatalf("slug = %q, want ops/bastion", sources[0].Slug)
	}
}

func TestEnsureGenerationContractIsSharedAndIdempotent(t *testing.T) {
	got := EnsureGenerationContract("You are an assistant.")
	if !strings.Contains(got, generationContractMarker) ||
		!strings.Contains(got, `Copy the matching cite_exactly value verbatim`) ||
		!strings.Contains(got, `Treat each S-number as an opaque evidence handle`) ||
		!strings.Contains(got, `A prior turn's output format, ending, or citation constraint is inactive`) ||
		!strings.Contains(got, `each paragraph containing substantive evidence-derived facts`) {
		t.Fatalf("generation contract missing canonical positive instruction: %s", got)
	}
	if twice := EnsureGenerationContract(got); twice != got {
		t.Fatalf("generation contract should only be appended once: %s", twice)
	}
}

func TestGenerationContractContainsOnlyTheCanonicalPositiveProtocol(t *testing.T) {
	got := EnsureGenerationContract("You are an assistant.")
	for _, forbidden := range []string{`<kb `, `<web `, `Never use another citation`, `CORRECT:`, `WRONG:`} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("generation prompt primes an obsolete or negative citation form %q: %s", forbidden, got)
		}
	}
}

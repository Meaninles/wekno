package sourcerefs

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func citationTestRefs() []*types.SearchResult {
	return []*types.SearchResult{
		{ID: "chunk-1", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText), Content: "甲事实", EvidenceContent: "甲事实", Metadata: map[string]string{MetadataCitationID: "S1", MetadataChunkID: "chunk-1", "source_type": SourceTypeKnowledge}},
		{ID: "chunk-2", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText), Content: "乙事实", EvidenceContent: "乙事实", Metadata: map[string]string{MetadataCitationID: "S2", MetadataChunkID: "chunk-2", "source_type": SourceTypeKnowledge}},
	}
}

func TestFilterAnswerCitationsKeepsOnlyCanonicalRegistryBackedSources(t *testing.T) {
	answer := `甲事实。<src id="S1" /> 错误来源。<src id="S999" /> ` +
		`错误协议。<doc id="1" source_id="S2" />`
	filtered, refs, report := FilterAnswerCitations(answer, citationTestRefs())
	if !strings.Contains(filtered, `<src id="S1" />`) {
		t.Fatalf("valid citation was removed: %q", filtered)
	}
	if strings.Contains(filtered, "S999") || strings.Contains(filtered, "<doc") {
		t.Fatalf("invalid citation survived: %q", filtered)
	}
	if len(refs) != 1 || CitationID(refs[0]) != "S1" {
		t.Fatalf("cited refs = %#v, want S1 only", refs)
	}
	if len(report.UnknownIDs) != 1 || report.UnknownIDs[0] != "S999" || report.ForbiddenTags != 1 {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestFilterAnswerCitationsDoesNotInterpretCodeExamples(t *testing.T) {
	answer := "示例：`<doc source_id=\"S1\" />`\n\n```xml\n<src id=\"S9\" />\n```"
	filtered, refs, report := FilterAnswerCitations(answer, citationTestRefs())
	if filtered != answer {
		t.Fatalf("code example changed:\n%s", filtered)
	}
	if len(refs) != 0 || len(report.UnknownIDs) != 0 || report.ForbiddenTags != 0 {
		t.Fatalf("code example was treated as citation: %#v", report)
	}
}

func TestFilterAnswerCitationsReportsEvidenceAvailableButUncited(t *testing.T) {
	filtered, refs, report := FilterAnswerCitations("基于证据生成的正文。", citationTestRefs())
	if filtered != "基于证据生成的正文。" || len(refs) != 0 {
		t.Fatalf("uncited answer should remain readable without invented references: filtered=%q refs=%#v", filtered, refs)
	}
	if report.AvailableCount != 2 || !report.EvidenceAvailableUncited {
		t.Fatalf("missing all-citations observability: %#v", report)
	}

	_, _, citedReport := FilterAnswerCitations("甲事实。<src id=\"S1\" />", citationTestRefs())
	if citedReport.AvailableCount != 2 || citedReport.EvidenceAvailableUncited {
		t.Fatalf("valid citation should clear the omission signal: %#v", citedReport)
	}
}

func TestFilterAnswerCitationsDropsMalformedOpeningClosingAndIncompleteTags(t *testing.T) {
	answer := `甲。<src id="S1"></src>乙。</doc><source id="S1" /><document source_id="S2" />` +
		`无空格。<src id="S1"/> 多余空格。<src  id="S1" />丙。<src id="S1"`
	filtered, refs, report := FilterAnswerCitations(answer, citationTestRefs())
	if strings.Contains(filtered, "<src") || strings.Contains(filtered, "</src") ||
		strings.Contains(filtered, "</doc") || strings.Contains(filtered, "<source") ||
		strings.Contains(filtered, "<document") {
		t.Fatalf("malformed citation markup survived: %q", filtered)
	}
	if len(refs) != 0 || report.ForbiddenTags < 6 || report.IncompleteTags != 1 {
		t.Fatalf("unexpected malformed-tag report: %#v", report)
	}
}

func TestFilterAnswerCitationsCollapsesOnlyAdjacentSameEvidence(t *testing.T) {
	answer := "甲。<src id=\"S1\" /> \n <src id=\"S1\" /><src id=\"S1\" />  <src id=\"S1\" />" +
		"<src id=\"S2\" />乙。<src id=\"S1\" />丙。<src id=\"S1\" />"
	filtered, refs, report := FilterAnswerCitations(answer, citationTestRefs())
	if got := strings.Count(filtered, `<src id="S1" />`); got != 3 {
		t.Fatalf("S1 count = %d, want 3 after adjacent-only collapse: %q", got, filtered)
	}
	if got := strings.Count(filtered, `<src id="S2" />`); got != 1 {
		t.Fatalf("S2 count = %d, want 1: %q", got, filtered)
	}
	if !strings.Contains(filtered, `<src id="S1" /><src id="S2" />`) {
		t.Fatalf("different adjacent sources must retain their order: %q", filtered)
	}
	if len(refs) != 2 || report.AdjacentDuplicates != 3 {
		t.Fatalf("unexpected refs/report: refs=%#v report=%#v", refs, report)
	}
}

func TestCollapseAdjacentDuplicateCitationsPreservesLaterRuns(t *testing.T) {
	tag := func(id string) string { return `<src id="S` + id + `" />` }
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "112233 becomes 123", in: tag("1") + tag("1") + tag("2") + tag("2") + tag("3") + tag("3"), want: tag("1") + tag("2") + tag("3")},
		{name: "122322 becomes 1232", in: tag("1") + tag("2") + tag("2") + tag("3") + tag("2") + tag("2"), want: tag("1") + tag("2") + tag("3") + tag("2")},
		{name: "112223444322222 becomes 123432", in: tag("1") + tag("1") + tag("2") + tag("2") + tag("2") + tag("3") + tag("4") + tag("4") + tag("4") + tag("3") + tag("2") + tag("2") + tag("2") + tag("2") + tag("2"), want: tag("1") + tag("2") + tag("3") + tag("4") + tag("3") + tag("2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var report CitationValidationReport
			if got := collapseAdjacentDuplicateCitations(tc.in, &report); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if report.AdjacentDuplicates != 3 && tc.name == "112233 becomes 123" {
				t.Fatalf("collapsed %d duplicates, want 3", report.AdjacentDuplicates)
			}
			if report.AdjacentDuplicates != 2 && tc.name == "122322 becomes 1232" {
				t.Fatalf("collapsed %d duplicates, want 2", report.AdjacentDuplicates)
			}
			if report.AdjacentDuplicates != 9 && tc.name == "112223444322222 becomes 123432" {
				t.Fatalf("collapsed %d duplicates, want 9", report.AdjacentDuplicates)
			}
		})
	}
}

func TestFilterAnswerCitationsMovesExactWholeListCitationAfterFinalItem(t *testing.T) {
	refs := []*types.SearchResult{{
		ID: "chunk-methods", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1",
		ChunkType:       string(types.ChunkTypeText),
		Content:         "采购方式有招标采购、询比采购、竞价采购、谈判采购、框架协议采购和单源采购六种。",
		EvidenceContent: "采购方式有招标采购、询比采购、竞价采购、谈判采购、框架协议采购和单源采购六种。",
		Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "chunk-methods", "source_type": SourceTypeKnowledge},
	}}
	answer := "采购方式共有六种<src id=\"S1\" />：\n\n1. **招标采购**\n2. **询比采购**\n3. **竞价采购**\n4. **谈判采购**\n5. **框架协议采购**\n6. **单源采购**"
	filtered, cited, report := FilterAnswerCitations(answer, refs)
	if strings.Index(filtered, `<src id="S1" />`) < strings.Index(filtered, "单源采购") {
		t.Fatalf("whole-list citation was not moved after the final item: %q", filtered)
	}
	if len(cited) != 1 || report.RelocatedListCitations != 1 || report.UnsupportedListCitations != 0 {
		t.Fatalf("unexpected cited refs/report: refs=%#v report=%#v", cited, report)
	}
}

func TestFilterAnswerCitationsCompletesRepeatedListAfterShortTransition(t *testing.T) {
	refs := []*types.SearchResult{{
		ID: "chunk-methods", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1",
		ChunkType:       string(types.ChunkTypeText),
		Content:         "采购方式有招标采购、询比采购、竞价采购、谈判采购、框架协议采购和单源采购六种。",
		EvidenceContent: "采购方式有招标采购、询比采购、竞价采购、谈判采购、框架协议采购和单源采购六种。",
		Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "chunk-methods", "source_type": SourceTypeKnowledge},
	}}
	answer := "第三十二条明确列出六种采购方式。<src id=\"S1\" />\n\n这六种采购方式分别为：\n\n1. **招标采购**\n2. **询比采购**\n3. **竞价采购**\n4. **谈判采购**\n5. **框架协议采购**\n6. **单源采购**"
	filtered, cited, report := FilterAnswerCitations(answer, refs)
	if strings.Count(filtered, `<src id="S1" />`) != 2 || strings.LastIndex(filtered, `<src id="S1" />`) < strings.Index(filtered, "单源采购") {
		t.Fatalf("repeated supported list did not receive a trailing citation: %q", filtered)
	}
	if len(cited) != 1 || report.CompletedListCitations != 1 || report.RelocatedListCitations != 0 {
		t.Fatalf("unexpected cited refs/report: refs=%#v report=%#v", cited, report)
	}
}

func TestFilterAnswerCitationsDoesNotCompleteListAfterLongOrStructuralBridge(t *testing.T) {
	refs := citationTestRefs()
	answer := "事实。<src id=\"S1\" />\n\n## 另一节：\n\n1. 第一类\n2. 第二类\n3. 第三类\n4. 第四类"
	filtered, cited, report := FilterAnswerCitations(answer, refs)
	if filtered != answer || len(cited) != 1 || report.CompletedListCitations != 0 {
		t.Fatalf("structural bridge must not acquire a citation: filtered=%q refs=%#v report=%#v", filtered, cited, report)
	}
}

func TestFilterAnswerCitationsDropsClearlyMismatchedWholeListCitation(t *testing.T) {
	refs := []*types.SearchResult{{
		ID: "chunk-thresholds", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1",
		ChunkType:       string(types.ChunkTypeText),
		Content:         "施工合同估算价达到400万元时必须公开招标。",
		EvidenceContent: "施工合同估算价达到400万元时必须公开招标。",
		Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "chunk-thresholds", "source_type": SourceTypeKnowledge},
	}}
	answer := "采购方式共有六种<src id=\"S2\" />：\n\n1. 招标采购\n2. 询比采购\n3. 竞价采购\n4. 谈判采购\n5. 框架协议采购\n6. 单源采购"
	filtered, cited, report := FilterAnswerCitations(answer, refs)
	if strings.Contains(filtered, "<src") || len(cited) != 0 || report.UnsupportedListCitations != 1 {
		t.Fatalf("clearly mismatched list citation survived: filtered=%q refs=%#v report=%#v", filtered, cited, report)
	}
}

func TestFilterAnswerCitationsLeavesShortAmbiguousListUntouched(t *testing.T) {
	refs := citationTestRefs()
	answer := "可分两类<src id=\"S1\" />：\n\n- 第一类\n- 第二类"
	filtered, cited, report := FilterAnswerCitations(answer, refs)
	if filtered != answer || len(cited) != 1 || report.RelocatedListCitations != 0 || report.UnsupportedListCitations != 0 {
		t.Fatalf("short ambiguous list should not be normalized: filtered=%q refs=%#v report=%#v", filtered, cited, report)
	}
}

func TestFilterAnswerCitationsDoesNotApplyDocumentListRulesToWebEvidence(t *testing.T) {
	refs := []*types.SearchResult{{
		ID:              "https://example.com/source",
		Content:         "网页证据",
		EvidenceContent: "网页证据",
		Metadata: map[string]string{
			MetadataCitationID: "S3",
			"source_type":      SourceTypeWeb,
			"url":              "https://example.com/source",
		},
	}}
	answer := "网页列出六项<src id=\"S3\" />：\n\n1. 第一项\n2. 第二项\n3. 第三项\n4. 第四项\n5. 第五项\n6. 第六项"
	filtered, cited, report := FilterAnswerCitations(answer, refs)
	if filtered != answer || len(cited) != 1 || report.RelocatedListCitations != 0 || report.UnsupportedListCitations != 0 {
		t.Fatalf("document-only list normalization changed web evidence: filtered=%q refs=%#v report=%#v", filtered, cited, report)
	}
}

func TestFilterAnswerCitationsMovesExactlySupportedWikiListCitation(t *testing.T) {
	refs := []*types.SearchResult{{
		ID:              "wiki:kb-1:concept/beiyongjin",
		KnowledgeBaseID: "kb-1",
		ChunkType:       "wiki_page",
		Content:         "备用金分为定额备用金和临时备用金两类。",
		EvidenceContent: "备用金分为定额备用金和临时备用金两类。",
		Metadata: map[string]string{
			MetadataCitationID: "S4",
			"source_type":      SourceTypeWiki,
			"slug":             "concept/beiyongjin",
		},
	}}
	answer := "备用金分为两类<src id=\"S4\" />：\n\n- **定额备用金**\n- **临时备用金**"
	filtered, cited, report := FilterAnswerCitations(answer, refs)
	if strings.Index(filtered, `<src id="S4" />`) < strings.Index(filtered, "临时备用金") {
		t.Fatalf("supported Wiki list citation was not moved after the final item: %q", filtered)
	}
	if len(cited) != 1 || report.RelocatedListCitations != 1 || report.UnsupportedListCitations != 0 {
		t.Fatalf("unexpected cited refs/report: refs=%#v report=%#v", cited, report)
	}
}

func TestStripCitationProtocolPreventsStaleSourceIDsInHistory(t *testing.T) {
	content := `事实。<src id="S2" /> [[ops/guide|运维指南]]`
	got := StripCitationProtocol(content)
	if strings.Contains(got, "S2") || strings.Contains(got, "[[") || !strings.Contains(got, "运维指南") {
		t.Fatalf("unexpected stripped history: %q", got)
	}
}

func TestDecodeSearchResultsPreservesSnapshotFields(t *testing.T) {
	input := []interface{}{map[string]interface{}{
		"id": "chunk-1", "content": "证据", "evidence_content": "精确证据", "chunk_type": "text",
		"source_locator": map[string]interface{}{"page": float64(3)},
		"metadata":       map[string]interface{}{"citation_id": "S1", "evidence_hash": "sha256:x"},
	}}
	refs := DecodeSearchResults(input)
	if len(refs) != 1 || refs[0].ChunkType != "text" || refs[0].EvidenceContent != "精确证据" || CitationID(refs[0]) != "S1" || len(refs[0].SourceLocator) == 0 {
		t.Fatalf("reference fields were lost: %#v", refs)
	}
}

func BenchmarkFilterAnswerCitations(b *testing.B) {
	refs := citationTestRefs()
	answer := strings.Repeat("正文内容。<src id=\"S1\" /> ", 200)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = FilterAnswerCitations(answer, refs)
	}
}

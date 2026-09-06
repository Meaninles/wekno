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

func TestFilterAnswerCitationsMovesHandlesOutsideURLsWithoutChangingTargets(t *testing.T) {
	answer := "应用入口：[打开控制台](https://portal.example.net/apps/view?id=42<src id=\"S1\" />)\n" +
		"备用入口：https://docs.example.net/guide#access<src id=\"S2\" />"
	filtered, refs, report := FilterAnswerCitations(answer, citationTestRefs())

	if !strings.Contains(filtered, `[打开控制台](https://portal.example.net/apps/view?id=42)<src id="S1" />`) {
		t.Fatalf("citation still corrupts Markdown target: %q", filtered)
	}
	if !strings.Contains(filtered, `https://docs.example.net/guide#access <src id="S2" />`) {
		t.Fatalf("citation still concatenates with bare URL: %q", filtered)
	}
	if len(refs) != 2 || report.RelocatedURLCitations != 2 {
		t.Fatalf("unexpected references/report: refs=%#v report=%#v", refs, report)
	}
}

func TestFilterAnswerCitationsRejoinsURLSplitByAHandle(t *testing.T) {
	answer := "主入口：[控制台](https://portal.example.net/open? <src id=\"S1\" />tenant=blue&view=summary)\n" +
		"备用入口：https://docs.example.net/guide?lang= <src id=\"S2\" />zh-CN#start"
	filtered, refs, report := FilterAnswerCitations(answer, citationTestRefs())

	if !strings.Contains(filtered, `[控制台](https://portal.example.net/open?tenant=blue&view=summary)<src id="S1" />`) {
		t.Fatalf("citation still splits Markdown URL: %q", filtered)
	}
	if !strings.Contains(filtered, `https://docs.example.net/guide?lang=zh-CN#start <src id="S2" />`) {
		t.Fatalf("citation still splits bare URL: %q", filtered)
	}
	if len(refs) != 2 || report.RelocatedURLCitations != 2 {
		t.Fatalf("unexpected references/report: refs=%#v report=%#v", refs, report)
	}
}

func TestFilterAnswerCitationsDoesNotRewriteURLExamplesInsideCode(t *testing.T) {
	answer := "`[example](https://host.invalid/path<src id=\"S1\" />)`\n\n" +
		"```md\nhttps://host.invalid/raw<src id=\"S2\" />\n```"
	filtered, refs, report := FilterAnswerCitations(answer, citationTestRefs())
	if filtered != answer || len(refs) != 0 || report.RelocatedURLCitations != 0 {
		t.Fatalf("code example changed: filtered=%q refs=%#v report=%#v", filtered, refs, report)
	}
}

func TestFilterAnswerCitationsDropsMalformedOpeningClosingAndIncompleteTags(t *testing.T) {
	answer := `甲。<src id="S1"></src>乙。</doc><source id="S1" /><document source_id="S2" />` +
		`无空格。<src id="S1"/> 多余空格。<src  id="S1" />丙。<src id="S1"`
	filtered, refs, report := FilterAnswerCitations(answer, citationTestRefs())
	expected := `甲。<src id="S1" />乙。无空格。<src id="S1" /> 多余空格。<src id="S1" />丙。`
	if filtered != expected || len(refs) != 1 || CitationID(refs[0]) != "S1" || report.IncompleteTags != 1 {
		t.Fatalf("registry-backed aliases must normalize without changing prose: %q %#v", filtered, report)
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

func TestRepairCitationProjectionNeverInfersBindings(t *testing.T) {
	refs := citationTestRefs()
	for _, body := range []string{"甲事实", "甲事实<src id=\"S2\" />", "甲事实<src id=\"S1\" />\n\n- 甲事实\n- 乙事实"} {
		got, _, _ := FilterAnswerCitations(body, refs)
		if got != body {
			t.Fatalf("claim/citation binding inferred: %q => %q", body, got)
		}
	}
	got, _, _ := FilterAnswerCitations("甲事实（S1）", refs)
	if got != "甲事实<src id=\"S1\" />" {
		t.Fatal(got)
	}
}

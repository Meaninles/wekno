package sourcerefs

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func citationTestRefs() []*types.SearchResult {
	return []*types.SearchResult{
		{ID: "chunk-1", Content: "甲事实", Metadata: map[string]string{MetadataCitationID: "S1"}},
		{ID: "chunk-2", Content: "乙事实", Metadata: map[string]string{MetadataCitationID: "S2"}},
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

func TestStripCitationProtocolPreventsStaleSourceIDsInHistory(t *testing.T) {
	content := `事实。<src id="S2" /> [[ops/guide|运维指南]]`
	got := StripCitationProtocol(content)
	if strings.Contains(got, "S2") || strings.Contains(got, "[[") || !strings.Contains(got, "运维指南") {
		t.Fatalf("unexpected stripped history: %q", got)
	}
}

func TestDecodeSearchResultsPreservesSnapshotFields(t *testing.T) {
	input := []interface{}{map[string]interface{}{
		"id": "chunk-1", "content": "证据", "chunk_type": "text",
		"source_locator": map[string]interface{}{"page": float64(3)},
		"metadata":       map[string]interface{}{"citation_id": "S1", "evidence_hash": "sha256:x"},
	}}
	refs := DecodeSearchResults(input)
	if len(refs) != 1 || refs[0].ChunkType != "text" || CitationID(refs[0]) != "S1" || len(refs[0].SourceLocator) == 0 {
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

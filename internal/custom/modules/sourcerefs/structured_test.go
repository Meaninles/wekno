package sourcerefs

import (
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestStructuredExplicitOffsetsPreserveRepeatedUnicodeMarkdown(t *testing.T) {
	text := "**中文😀。**"
	body := text + "\n\n" + text
	raw, err := json.Marshal([]StructuredCitation{{Text: text, End: len(text), SourceIDs: []string{"S1"}}, {Text: text, End: len(body), SourceIDs: []string{"S2"}}})
	require.NoError(t, err)
	answer, used, citations := RenderStructuredCitations(body, raw, structuredTestRefs())
	require.Equal(t, text+`<src id="S1" />`+"\n\n"+text+`<src id="S2" />`, answer)
	require.Equal(t, body, citationLikeTagRE.ReplaceAllString(answer, ""))
	require.Len(t, used, 2)
	require.Len(t, citations, 2)
	invalid, err := json.Marshal([]StructuredCitation{{Text: text, End: 1, SourceIDs: []string{"S1"}}})
	require.NoError(t, err)
	answer, used, citations = RenderStructuredCitations(body, invalid, structuredTestRefs())
	require.Equal(t, body, answer)
	require.Empty(t, used)
	require.Empty(t, citations)
}

func TestStructuredEvidenceOnlyIncludesUsableRegisteredEvidence(t *testing.T) {
	refs := structuredTestRefs()
	refs[1].EvidenceContent = ""
	refs[0].Content = "not evidence"
	evidence := StructuredEvidence(append(refs, refs[0], nil))
	require.Len(t, evidence, 2)
	require.Equal(t, "原文 A", evidence[0]["text"])
	require.Len(t, evidence[0], 3)
}

func structuredTestRefs() []*types.SearchResult {
	refs := []*types.SearchResult{
		{ID: "chunk-a", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: "text", Content: "原文 A", EvidenceContent: "原文 A"},
		{ID: "chunk-b", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: "text", Content: "原文 B", EvidenceContent: "原文 B"},
		{ID: "chunk-c", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: "text", Content: "原文 C", EvidenceContent: "原文 C"},
	}
	AssignCitationIDs(refs)
	return refs
}

func TestStructuredCitationsMergeAndFirstOccurrence(t *testing.T) {
	refs := structuredTestRefs()
	answer, used, filtered := RenderStructuredCitations("正文A。正文B。正文A。", json.RawMessage(`[
		{"text":"正文A。","source_ids":["S1"]},
		{"text":"正文A。","source_ids":["S1","S2","S999",null,17]},
		{"text":"正文B。","source_ids":["S3"]}
	]`), refs)
	require.Equal(t, `正文A。<src id="S1" /><src id="S2" />正文B。<src id="S3" />正文A。`, answer)
	require.Equal(t, []StructuredCitation{{"正文A。", []string{"S1", "S2"}, 0}, {"正文B。", []string{"S3"}, 0}}, filtered)
	require.Equal(t, refs, used)
}

func TestStructuredCitationsFilterOnlyMissingData(t *testing.T) {
	refs := structuredTestRefs()
	refs[1].EvidenceContent = ""
	answer, used, filtered := RenderStructuredCitations("任意结论。", json.RawMessage(`[
		null, 7, "bad", {}, {"text":7,"source_ids":["S1"]},
		{"text":"","source_ids":["S1"]},
		{"text":"不存在","source_ids":["S1"]},
		{"text":"任意结论。","source_ids":"S1"},
		{"text":"任意结论。","source_ids":["chunk-a","S2","S999"]},
		{"text":"任意结论。","source_ids":[17,"S3"]}
	]`), refs)
	// Semantic support is deliberately not evaluated, only exact anchoring/data.
	require.Equal(t, `任意结论。<src id="S3" />`, answer)
	require.Equal(t, []*types.SearchResult{refs[2]}, used)
	require.Equal(t, []StructuredCitation{{"任意结论。", []string{"S3"}, 0}}, filtered)
	for _, raw := range []string{"", "null", `{}`, `"bad"`, "[]"} {
		got, used, items := RenderStructuredCitations("保留正文。", json.RawMessage(raw), refs)
		require.Equal(t, "保留正文。", got)
		require.Empty(t, used)
		require.Empty(t, items)
	}
}

func TestStructuredCitationsOriginalOffsetsAndBodyOrder(t *testing.T) {
	refs := structuredTestRefs()
	answer, used, _ := RenderStructuredCitations("甲乙。\n\n尾。", json.RawMessage(`[
		{"text":"尾。","source_ids":["S1"]},
		{"text":"甲乙。","source_ids":["S3"]},
		{"text":"甲乙","source_ids":["S2"]}
	]`), refs)
	require.Equal(t, "甲乙<src id=\"S2\" />。<src id=\"S3\" />\n\n尾。<src id=\"S1\" />", answer)
	require.Equal(t, []*types.SearchResult{refs[1], refs[2], refs[0]}, used)
	// Unknown legacy tags cannot create a live reference; code examples survive.
	answer, used, _ = RenderStructuredCitations("正文<src id=\"S999\" /> `literal <src id=\"S1\" />`", nil, refs)
	require.Equal(t, "正文 `literal <src id=\"S1\" />`", answer)
	require.Empty(t, used)
}

func TestStructuredCatalogIsAuthoritativeAndPromptIsSingleContract(t *testing.T) {
	refs := structuredTestRefs()
	refs[1].EvidenceContent = ""
	catalog := StructuredCatalog(append(refs, refs[0], nil))
	require.Len(t, catalog, 2)
	require.Equal(t, "S1", catalog[0]["id"])
	require.Equal(t, "chunk-a", catalog[0]["chunk_id"])
	prompt := EnsureStructuredContract(EnsureGenerationContract("instructions") + TerminalCitationInstruction())
	require.Equal(t, prompt, EnsureStructuredContract(prompt))
	require.NotContains(t, prompt, "[CITATION_USE]")
	require.NotContains(t, prompt, "[WEKNORA_CITATION_OUTPUT]")
	require.Contains(t, prompt, "[CURRENT_RUN_SOURCES]")
}

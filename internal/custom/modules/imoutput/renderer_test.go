package imoutput

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestRenderMixedReferencesUsesAnswerOccurrenceOrder(t *testing.T) {
	refs := []*types.SearchResult{
		citationRef("S1", sourcerefs.SourceTypeKnowledge, map[string]string{
			"knowledge_base_id": "kb-1", "knowledge_id": "doc-1", "chunk_id": "chunk-1",
		}),
		citationRef("S2", sourcerefs.SourceTypeWiki, map[string]string{
			"knowledge_base_id": "kb-1", "slug": "ops/runbook",
		}),
		citationRef("S3", sourcerefs.SourceTypeWeb, map[string]string{
			"url": "https://example.com/a?q=1",
		}),
	}
	result := Render(
		"网页事实。<src id=\"S3\" /> 文档事实。<src id=\"S1\" /> 再次引用。<src id=\"S3\" /> Wiki。<src id=\"S2\" />",
		refs,
		testRenderOptions("wecom", true),
	)
	if len(result.References) != 3 {
		t.Fatalf("references=%d, want 3: %#v", len(result.References), result.References)
	}
	if result.References[0].CitationID != "S3" || result.References[1].CitationID != "S1" || result.References[2].CitationID != "S2" {
		t.Fatalf("wrong occurrence order: %#v", result.References)
	}
	if strings.Count(result.Content, `[[1](https://example.com/a?q=1)]`) != 2 {
		t.Fatalf("repeated source did not reuse number: %s", result.Content)
	}
	documentCapability := capabilityFromTarget(t, result.References[1].Target)
	if documentCapability.KnowledgeBaseID != "kb-1" || documentCapability.KnowledgeID != "doc-1" || documentCapability.ChunkID != "chunk-1" {
		t.Fatalf("document capability lost exact chunk: %#v", documentCapability)
	}
	if !strings.Contains(result.References[1].Target, ReferenceRedirectPath) ||
		!strings.Contains(result.References[2].Target, ReferenceRedirectPath) {
		t.Fatalf("internal references must use the device-neutral IM entry: %#v", result.References)
	}
	if strings.Contains(result.Content, "参考来源") {
		t.Fatalf("IM output must not append a bottom reference list: %s", result.Content)
	}
}

func TestRenderKeepsDistinctChunksInOneDocument(t *testing.T) {
	refs := []*types.SearchResult{
		citationRef("S1", sourcerefs.SourceTypeKnowledge, map[string]string{
			"knowledge_base_id": "kb", "knowledge_id": "doc", "chunk_id": "c1",
		}),
		citationRef("S2", sourcerefs.SourceTypeKnowledge, map[string]string{
			"knowledge_base_id": "kb", "knowledge_id": "doc", "chunk_id": "c2",
		}),
	}
	result := Render("甲。<src id=\"S1\" />乙。<src id=\"S2\" />", refs, testRenderOptions("wecom", false))
	if len(result.References) != 2 || result.References[0].Target == result.References[1].Target {
		t.Fatalf("distinct chunks collapsed: %#v", result.References)
	}
}

func TestRenderDoesNotEmitRelativeDocumentLinks(t *testing.T) {
	refs := []*types.SearchResult{citationRef("S1", sourcerefs.SourceTypeKnowledge, map[string]string{
		"knowledge_base_id": "kb", "knowledge_id": "doc", "chunk_id": "c1",
	})}
	options := testRenderOptions("wecom", false)
	options.FrontendBaseURL = ""
	result := Render("甲。<src id=\"S1\" />", refs, options)
	if len(result.References) != 0 || strings.Contains(result.Content, ReferenceRedirectPath) {
		t.Fatalf("relative IM target leaked: %#v content=%s", result.References, result.Content)
	}
}

func TestRenderDropsUnsupportedOrInexactTargetsWithoutDowngrade(t *testing.T) {
	refs := []*types.SearchResult{
		citationRef("S1", sourcerefs.SourceTypeKnowledge, map[string]string{
			"knowledge_base_id": "kb", "knowledge_id": "doc",
		}),
		citationRef("S2", sourcerefs.SourceTypeData, map[string]string{"source_id": "db-1"}),
	}
	result := Render("甲。<src id=\"S1\" />乙。<src id=\"S2\" />", refs, Options{Platform: "wecom"})
	if len(result.References) != 0 || strings.Contains(result.Content, "<src") || strings.Contains(result.Content, "参考来源") {
		t.Fatalf("unsupported targets leaked: %#v content=%s", result.References, result.Content)
	}
}

func TestRenderDoesNotConvertCodeExamples(t *testing.T) {
	refs := []*types.SearchResult{citationRef("S1", sourcerefs.SourceTypeWeb, map[string]string{"url": "https://example.com"})}
	result := Render("事实。<src id=\"S1\" />\n`<src id=\"S1\" />`", refs, Options{Platform: "wecom"})
	if !strings.Contains(result.Content, "`<src id=\"S1\" />`") {
		t.Fatalf("code example was converted: %s", result.Content)
	}
}

func TestRenderDialectsAreAlwaysApplied(t *testing.T) {
	refs := []*types.SearchResult{citationRef("S1", sourcerefs.SourceTypeWeb, map[string]string{"url": "https://example.com"})}
	answer := "事实。<src id=\"S1\" />"

	slack := Render(answer, refs, Options{Platform: "slack"})
	if !strings.Contains(slack.Content, "<https://example.com|[1]>") || slack.Dialect != DialectSlack {
		t.Fatalf("unexpected Slack output: %s", slack.Content)
	}
	feishu := Render(answer, refs, Options{Platform: "feishu", Streaming: false})
	if feishu.Content != "事实。[[1](https://example.com)]" || feishu.Dialect != DialectFeishu {
		t.Fatalf("unexpected Feishu output: %s", feishu.Content)
	}
	wecom := Render(answer, refs, Options{Platform: "wecom"})
	if !strings.Contains(wecom.Content, `[[1](https://example.com)]`) || wecom.Dialect != DialectWeCom || len(wecom.References) != 1 {
		t.Fatalf("WeCom conversion must be unconditional: %#v", wecom)
	}
}

func TestWeComAndFeishuBracketLinksCoverAllVisibleReferenceTypes(t *testing.T) {
	refs := []*types.SearchResult{
		citationRef("S1", sourcerefs.SourceTypeKnowledge, map[string]string{
			"knowledge_base_id": "kb-1", "knowledge_id": "doc-1", "chunk_id": "chunk-1",
		}),
		citationRef("S2", sourcerefs.SourceTypeWiki, map[string]string{
			"knowledge_base_id": "kb-1", "slug": "ops/runbook",
		}),
		citationRef("S3", sourcerefs.SourceTypeWeb, map[string]string{
			"url": "https://example.com/source",
		}),
	}
	answer := `文档。<src id="S1" /> Wiki。<src id="S2" /> 网页。<src id="S3" />`

	for _, platform := range []string{"wecom", "feishu"} {
		for _, streaming := range []bool{false, true} {
			result := Render(answer, refs, testRenderOptions(platform, streaming))
			if len(result.References) != 3 {
				t.Fatalf("platform=%s streaming=%v refs=%#v", platform, streaming, result.References)
			}
			for index, sourceType := range []string{
				sourcerefs.SourceTypeKnowledge,
				sourcerefs.SourceTypeWiki,
				sourcerefs.SourceTypeWeb,
			} {
				if result.References[index].Type != sourceType {
					t.Fatalf("platform=%s reference %d type=%s", platform, index+1, result.References[index].Type)
				}
				marker := "[[" + string(rune('1'+index)) + "](" + result.References[index].Target + ")]"
				if !strings.Contains(result.Content, marker) {
					t.Fatalf("platform=%s missing bracket link %q in %q", platform, marker, result.Content)
				}
			}
		}
	}
}

func TestRenderFiltersInvalidTagsWithoutRegeneration(t *testing.T) {
	refs := []*types.SearchResult{citationRef("S1", sourcerefs.SourceTypeWeb, map[string]string{"url": "https://example.com"})}
	result := Render("甲<kb doc=\"x\">乙<src id=\"S9\" />丙<src id=\"S1\" />", refs, Options{Platform: "wecom"})
	if result.Validation.ForbiddenTags != 1 || len(result.Validation.UnknownIDs) != 1 || len(result.References) != 1 {
		t.Fatalf("validation mismatch: %#v", result)
	}
	if strings.Contains(result.Content, "<kb") || strings.Contains(result.Content, "S9") {
		t.Fatalf("invalid protocol leaked: %s", result.Content)
	}
}

func citationRef(id, sourceType string, metadata map[string]string) *types.SearchResult {
	copyMetadata := map[string]string{
		sourcerefs.MetadataCitationID:    id,
		sourcerefs.MetadataCitationTitle: "来源 " + id,
		"source_type":                    sourceType,
	}
	for key, value := range metadata {
		copyMetadata[key] = value
	}
	return &types.SearchResult{
		ID:              copyMetadata["chunk_id"],
		KnowledgeID:     copyMetadata["knowledge_id"],
		KnowledgeBaseID: copyMetadata["knowledge_base_id"],
		KnowledgeTitle:  copyMetadata[sourcerefs.MetadataCitationTitle],
		Metadata:        copyMetadata,
	}
}

func testRenderOptions(platform string, streaming bool) Options {
	return Options{
		FrontendBaseURL: "https://knora.example.com",
		Platform:        platform,
		Streaming:       streaming,
		TenantID:        7,
		ReferenceSigner: NewReferenceSigner([]byte("0123456789abcdef0123456789abcdef")),
	}
}

func capabilityFromTarget(t *testing.T, target string) ReferenceCapability {
	t.Helper()
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse target %q: %v", target, err)
	}
	token := parsed.Query().Get("token")
	if token == "" {
		t.Fatalf("target has no capability token: %s", target)
	}
	capability, err := NewReferenceSigner([]byte("0123456789abcdef0123456789abcdef")).Verify(token)
	if err != nil {
		t.Fatalf("verify target capability: %v", err)
	}
	return capability
}

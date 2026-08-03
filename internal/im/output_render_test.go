package im

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/imoutput"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestRenderFinalOutboundUsesOneBoundaryForEveryPlatform(t *testing.T) {
	service := &Service{
		frontendBaseURL: "https://knora.example.com",
		referenceSigner: imoutput.NewReferenceSigner([]byte("0123456789abcdef0123456789abcdef")),
	}
	tenant := &types.Tenant{ID: 7}
	refs := []*types.SearchResult{{
		ID:              "chunk-1",
		KnowledgeID:     "doc-1",
		KnowledgeBaseID: "kb-1",
		KnowledgeTitle:  "制度",
		Metadata: map[string]string{
			sourcerefs.MetadataCitationID:    "S1",
			sourcerefs.MetadataCitationTitle: "制度分片",
			sourcerefs.MetadataChunkID:       "chunk-1",
			"source_type":                    sourcerefs.SourceTypeKnowledge,
		},
	}}
	answer := "结论。<src id=\"S1\" />"

	tests := []struct {
		name      string
		platform  Platform
		streaming bool
		dialect   imoutput.Dialect
		contains  string
	}{
		{name: "wecom application full", platform: PlatformWeCom, dialect: imoutput.DialectWeCom, contains: `[[1](`},
		{name: "wecom bot stream", platform: PlatformWeCom, streaming: true, dialect: imoutput.DialectWeCom, contains: `[[1](`},
		{name: "feishu full", platform: PlatformFeishu, dialect: imoutput.DialectFeishu, contains: `[[1](`},
		{name: "feishu card stream", platform: PlatformFeishu, streaming: true, dialect: imoutput.DialectFeishu, contains: `[[1](`},
		{name: "slack", platform: PlatformSlack, streaming: true, dialect: imoutput.DialectSlack, contains: "|[1]>"},
		{name: "telegram", platform: PlatformTelegram, streaming: true, dialect: imoutput.DialectMarkdown, contains: `[\[1\]](`},
		{name: "dingtalk", platform: PlatformDingtalk, dialect: imoutput.DialectMarkdown, contains: `[\[1\]](`},
		{name: "mattermost", platform: PlatformMattermost, streaming: true, dialect: imoutput.DialectMarkdown, contains: `[\[1\]](`},
		{name: "wechat", platform: PlatformWeChat, dialect: imoutput.DialectPlain, contains: "结论。[1]"},
		{name: "qqbot", platform: PlatformQQBot, dialect: imoutput.DialectPlain, contains: "结论。[1]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := service.RenderFinalOutbound(context.Background(), answer, refs, tenant, test.platform, test.streaming)
			if result.Dialect != test.dialect || !strings.Contains(result.Content, test.contains) {
				t.Fatalf("dialect=%s content=%q", result.Dialect, result.Content)
			}
			if len(result.References) != 1 {
				t.Fatalf("exact reference missing: %#v", result.References)
			}
			parsed, err := url.Parse(result.References[0].Target)
			if err != nil {
				t.Fatal(err)
			}
			capability, err := service.referenceSigner.Verify(parsed.Query().Get("token"))
			if err != nil || capability.TenantID != 7 || capability.KnowledgeID != "doc-1" || capability.ChunkID != "chunk-1" {
				t.Fatalf("exact signed capability missing: capability=%#v err=%v target=%s", capability, err, result.References[0].Target)
			}
			if strings.Contains(result.References[0].Target, "chunk-1") || strings.Contains(result.References[0].Target, "knowledge-bases") {
				t.Fatalf("IM target leaked raw coordinates or authenticated route: %s", result.References[0].Target)
			}
			if strings.Contains(result.Content, "<src") {
				t.Fatalf("canonical protocol leaked: %s", result.Content)
			}
			if strings.Contains(result.Content, "参考来源") {
				t.Fatalf("bottom reference list leaked: %s", result.Content)
			}
		})
	}
}

func TestRenderFinalOutboundAlwaysConvertsValidatedReferences(t *testing.T) {
	service := &Service{frontendBaseURL: "https://knora.example.com"}
	refs := []*types.SearchResult{{
		ID: "https://example.com",
		Metadata: map[string]string{
			sourcerefs.MetadataCitationID: "S1",
			"source_type":                 sourcerefs.SourceTypeWeb,
			"url":                         "https://example.com",
		},
	}}
	result := service.RenderFinalOutbound(
		context.Background(), "结论。<src id=\"S1\" />", refs, nil, PlatformWeCom, true,
	)
	if result.Content != `结论。[[1](https://example.com)]` || len(result.References) != 1 {
		t.Fatalf("always-on output=%q refs=%#v", result.Content, result.References)
	}
}

func TestStreamingKeepsCanonicalProtocolHiddenUntilFinalReplace(t *testing.T) {
	service := &Service{frontendBaseURL: "https://knora.example.com"}
	refs := []*types.SearchResult{{
		ID: "https://example.com/source",
		Metadata: map[string]string{
			sourcerefs.MetadataCitationID:    "S1",
			sourcerefs.MetadataCitationTitle: "网页来源",
			"source_type":                    sourcerefs.SourceTypeWeb,
			"url":                            "https://example.com/source",
		},
	}}
	raw := "流式正文。<src id=\"S1\" />"

	intermediate := cleanIMContent(
		context.Background(),
		FormatIMDisplayContent(raw, StreamDisplayIntermediate),
		nil,
		nil,
	)
	if strings.Contains(intermediate, "<src") || strings.Contains(intermediate, "参考来源") {
		t.Fatalf("intermediate frame leaked citation protocol or a premature source list: %q", intermediate)
	}

	final := service.RenderFinalOutbound(context.Background(), raw, refs, nil, PlatformWeCom, true)
	if !strings.Contains(final.Content, `[[1](https://example.com/source)]`) ||
		strings.Contains(final.Content, "参考来源") ||
		strings.Contains(final.Content, "<src") {
		t.Fatalf("final replace did not render stable clickable references: %q", final.Content)
	}
}

func BenchmarkRenderFinalOutbound(b *testing.B) {
	service := &Service{frontendBaseURL: "https://knora.example.com"}
	refs := []*types.SearchResult{{
		ID: "https://example.com",
		Metadata: map[string]string{
			sourcerefs.MetadataCitationID: "S1",
			"source_type":                 sourcerefs.SourceTypeWeb,
			"url":                         "https://example.com",
		},
	}}
	answer := strings.Repeat("回答正文。", 1000) + `<src id="S1" />`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = service.RenderFinalOutbound(context.Background(), answer, refs, nil, PlatformWeCom, true)
	}
}

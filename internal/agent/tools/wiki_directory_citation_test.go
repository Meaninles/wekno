package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type directoryCitationWikiService struct {
	interfaces.WikiPageService
	page     *types.WikiPage
	overview *types.WikiIndexResponse
	err      error
}

func (s *directoryCitationWikiService) GetPageBySlug(context.Context, string, string) (*types.WikiPage, error) {
	return s.page, nil
}

func (s *directoryCitationWikiService) GetIndexView(context.Context, string, []string, int, string) (*types.WikiIndexResponse, error) {
	return s.overview, s.err
}

func TestWikiDirectoryCitationThroughToolAndAnswer(t *testing.T) {
	svc := &directoryCitationWikiService{
		page: &types.WikiPage{
			ID: "directory-page", KnowledgeBaseID: "kb-one", Slug: "index",
			Title: "Directory", PageType: types.WikiPageTypeIndex, Version: 7,
			Content: "outdated stored directory", Summary: "outdated summary",
		},
		overview: &types.WikiIndexResponse{
			Intro: "outdated stored directory",
			Groups: []types.WikiIndexGroup{{
				Type: types.WikiPageTypeEntity, Total: 26,
				Items: []types.WikiIndexEntry{{Slug: "entity/example", Title: "Example", Summary: "Current entry"}},
			}},
		},
	}
	tool := NewWikiReadPageTool(svc, nil, NewWikiScopesFromKBIDs([]string{"kb-one"}))
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"slugs":["index"]}`))
	if err != nil || !result.Success {
		t.Fatalf("read failed: %v, %+v", err, result)
	}
	refs, sources := sourcerefs.RegisterToolResult(sourcerefs.NewRegistry(), ToolWikiReadPage, result)
	if len(refs) != 1 || len(sources) != 1 {
		t.Fatalf("live directory did not produce exactly one reference: %+v", refs)
	}
	if sources[0].Slug != "index" || sources[0].KnowledgeBaseID != "kb-one" {
		t.Fatalf("directory citation lost its destination: %+v", sources[0])
	}
	for _, want := range []string{"26 total, showing top 1", "Current entry", "Read individual pages"} {
		if !strings.Contains(refs[0].EvidenceContent, want) {
			t.Fatalf("missing live evidence %q: %s", want, refs[0].EvidenceContent)
		}
	}
	if strings.Contains(refs[0].EvidenceContent, "outdated") {
		t.Fatal("citation used stale directory content")
	}
	annotated := sourcerefs.AppendCitationCatalog(result.Output, refs)
	if !strings.Contains(annotated, sources[0].CiteExactly) {
		t.Fatal("model-visible directory has no copyable citation handle")
	}
	answer := "The directory lists 26 entities. " + sources[0].CiteExactly
	filtered, cited, report := sourcerefs.FilterAnswerCitations(answer, refs)
	if filtered != answer || len(cited) != 1 || len(report.UnknownIDs) != 0 {
		t.Fatalf("directory citation was dropped at answer boundary: %s %+v", filtered, report)
	}
}

func TestWikiUnavailableDirectoryAndLogAreNotCitationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, pageType string
		err            error
	}{
		{"directory error", types.WikiPageTypeIndex, errors.New("directory read failed")},
		{"directory absent", types.WikiPageTypeIndex, nil},
		{"log", types.WikiPageTypeLog, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &directoryCitationWikiService{
				page: &types.WikiPage{ID: "page", KnowledgeBaseID: "kb-one", Slug: "index", Title: "Page", PageType: tc.pageType, Content: "Stored content"},
				err:  tc.err,
			}
			tool := NewWikiReadPageTool(svc, nil, NewWikiScopesFromKBIDs([]string{"kb-one"}))
			result, err := tool.Execute(context.Background(), json.RawMessage(`{"slugs":["index"]}`))
			if err != nil {
				t.Fatal(err)
			}
			refs, _ := sourcerefs.RegisterToolResult(sourcerefs.NewRegistry(), ToolWikiReadPage, result)
			if len(refs) != 0 {
				t.Fatalf("unavailable directory or log became evidence: %+v", refs)
			}
		})
	}
}

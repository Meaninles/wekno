package imoutput

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/gin-gonic/gin"
)

var referenceTestKey = []byte("0123456789abcdef0123456789abcdef")

func TestReferenceRedirectUsesIsolatedDesktopReader(t *testing.T) {
	token := issueReferenceTestToken(t, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1",
	})
	location := runReferenceRedirect(t, token,
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) wxwork/4.1.31", "")
	assertReferenceLocation(t, location, "/im-reference", token)
	if strings.Contains(location, "knowledge-bases") || strings.Contains(location, "chunk-1") {
		t.Fatalf("public IM redirect leaked into the authenticated knowledge-base route: %s", location)
	}
}

func TestReferenceRedirectUsesIsolatedMobileReader(t *testing.T) {
	token := issueReferenceTestToken(t, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1",
	})
	location := runReferenceRedirect(t, token,
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Mobile/15E148 MicroMessenger/8.0 wxwork/4.1.31", "")
	assertReferenceLocation(t, location, "/mobile/reference", token)
}

func TestReferenceRedirectRoutesWikiForBothDevices(t *testing.T) {
	token := issueReferenceTestToken(t, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeWiki, KnowledgeBaseID: "wiki-kb", Slug: "concept/reserve",
	})
	desktop := runReferenceRedirect(t, token, "Mozilla/5.0 (Macintosh; Intel Mac OS X) wxwork/4.1", "")
	assertReferenceLocation(t, desktop, "/im-reference", token)
	mobile := runReferenceRedirect(t, token, "Mozilla/5.0", "?1")
	assertReferenceLocation(t, mobile, "/mobile/reference", token)
}

func TestReferenceRedirectRejectsRawCoordinatesTamperingAndArbitraryTargets(t *testing.T) {
	valid := issueReferenceTestToken(t, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeWiki, KnowledgeBaseID: "wiki-kb", Slug: "concept/reserve",
	})
	invalid := []string{
		ReferenceRedirectPath + "?type=knowledge&knowledge_base_id=kb&knowledge_id=doc&chunk_id=chunk",
		ReferenceRedirectPath + "?target=https%3A%2F%2Fevil.example",
		ReferenceRedirectPath + "?token=" + url.QueryEscape(valid[:len(valid)-1]+"A"),
	}
	for _, raw := range invalid {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, raw, nil)
		testReferenceHandler().Redirect(ctx)
		if recorder.Code == http.StatusFound || recorder.Header().Get("Location") != "" {
			t.Fatalf("raw=%s status=%d location=%q", raw, recorder.Code, recorder.Header().Get("Location"))
		}
	}
}

func runReferenceRedirect(t *testing.T, token, userAgent, clientHint string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, ReferenceRedirectPath+"?token="+url.QueryEscape(token), nil)
	ctx.Request.Header.Set("User-Agent", userAgent)
	if clientHint != "" {
		ctx.Request.Header.Set("Sec-CH-UA-Mobile", clientHint)
	}
	testReferenceHandler().Redirect(ctx)
	if recorder.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store, max-age=0" {
		t.Fatalf("Cache-Control=%q", got)
	}
	if got := recorder.Header().Get("Vary"); got != "User-Agent, Sec-CH-UA-Mobile" {
		t.Fatalf("Vary=%q", got)
	}
	if got := recorder.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy=%q", got)
	}
	return recorder.Header().Get("Location")
}

func assertReferenceLocation(t *testing.T, location, wantPath, wantToken string) {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse location %q: %v", location, err)
	}
	if parsed.Path != wantPath || parsed.Query().Get("token") != wantToken || len(parsed.Query()) != 1 {
		t.Fatalf("location=%s want path=%s with exact token", location, wantPath)
	}
}

func issueReferenceTestToken(t *testing.T, source *sourcerefs.CitationSource) string {
	t.Helper()
	token, err := NewReferenceSigner(referenceTestKey).Issue(source, 7)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func testReferenceHandler() *ReferenceHandler {
	return NewReferenceHandler(NewReferenceService(nil, nil, NewReferenceSigner(referenceTestKey)))
}

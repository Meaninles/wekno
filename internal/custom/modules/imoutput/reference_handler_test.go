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

func TestReferenceHandlerServesAnonymousReadSnapshotWithoutRawCoordinates(t *testing.T) {
	service, _, _ := newReferenceTestService(t)
	token := issueServiceToken(t, service.signer, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1",
	}, 7)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, ReferenceDataPath+"?token="+url.QueryEscape(token), nil)
	NewReferenceHandler(service).Data(ctx)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "精确分片正文") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "tenant_id") || strings.Contains(recorder.Body.String(), "knowledge_base_id") ||
		strings.Contains(recorder.Body.String(), "doc-1") || strings.Contains(recorder.Body.String(), "chunk-1") {
		t.Fatalf("public snapshot leaked internal coordinates: %s", recorder.Body.String())
	}
	assertPublicReferenceHeaders(t, recorder)
}

func TestReferenceHandlerOriginalUsesSameDocumentCapability(t *testing.T) {
	service, _, _ := newReferenceTestService(t)
	token := issueServiceToken(t, service.signer, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1",
	}, 7)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, ReferenceOriginalPath+"?token="+url.QueryEscape(token), nil)
	NewReferenceHandler(service).Original(ctx)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "original document" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Fatalf("Content-Disposition=%q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type=%q", got)
	}
	assertPublicReferenceHeaders(t, recorder)
}

func TestReferenceHandlerOriginalDownloadUsesAttachmentWithoutChangingCapability(t *testing.T) {
	service, _, _ := newReferenceTestService(t)
	token := issueServiceToken(t, service.signer, &sourcerefs.CitationSource{
		Type: sourcerefs.SourceTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeID: "doc-1", ChunkID: "chunk-1",
	}, 7)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodGet,
		ReferenceOriginalPath+"?download=1&token="+url.QueryEscape(token),
		nil,
	)
	NewReferenceHandler(service).Original(ctx)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "original document" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("Content-Disposition=%q", got)
	}
	assertPublicReferenceHeaders(t, recorder)
}

func TestReferenceHandlerNeverAcceptsUnsignedCoordinates(t *testing.T) {
	service, _, _ := newReferenceTestService(t)
	handler := NewReferenceHandler(service)
	for _, raw := range []string{
		ReferenceDataPath + "?knowledge_id=doc-1&chunk_id=chunk-1",
		ReferenceOriginalPath + "?token=not-signed",
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, raw, nil)
		if strings.HasPrefix(raw, ReferenceOriginalPath) {
			handler.Original(ctx)
		} else {
			handler.Data(ctx)
		}
		if recorder.Code == http.StatusOK {
			t.Fatalf("unsigned request succeeded: %s", raw)
		}
	}
}

func assertPublicReferenceHeaders(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Header().Get("Cache-Control") != "private, no-store, max-age=0" ||
		recorder.Header().Get("Referrer-Policy") != "no-referrer" ||
		recorder.Header().Get("X-Robots-Tag") != "noindex, nofollow, noarchive" {
		t.Fatalf("missing public capability safety headers: %#v", recorder.Header())
	}
}

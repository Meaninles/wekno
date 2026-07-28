package mobiledocument

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

type fakeKnowledgeFiles struct {
	record       *types.Knowledge
	body         string
	openedTenant uint64
}

func (f *fakeKnowledgeFiles) GetKnowledgeByIDOnly(_ context.Context, id string) (*types.Knowledge, error) {
	if f.record != nil && f.record.ID == id {
		copyRecord := *f.record
		return &copyRecord, nil
	}
	return nil, ErrInvalidTicket
}

func (f *fakeKnowledgeFiles) GetKnowledgeFile(ctx context.Context, id string) (io.ReadCloser, string, error) {
	if f.record == nil || f.record.ID != id {
		return nil, "", ErrInvalidTicket
	}
	f.openedTenant, _ = ctx.Value(types.TenantIDContextKey).(uint64)
	return io.NopCloser(strings.NewReader(f.body)), f.record.FileName, nil
}

func TestSignedDownloadStreamsAttachmentForOwnerTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)
	files := &fakeKnowledgeFiles{
		record: &types.Knowledge{
			ID:       "knowledge-1",
			TenantID: 10023,
			FileName: "企业文档.pdf",
			FileSize: int64(len("pdf-body")),
			Type:     "file",
		},
		body: "pdf-body",
	}
	service := NewService(files, Config{
		SigningKey: []byte("0123456789abcdef0123456789abcdef"),
		TTL:        2 * time.Minute,
	})
	service.now = func() time.Time { return now }
	handler := NewHandler(service)

	issueRouter := gin.New()
	issueRouter.POST("/knowledge/:knowledge_id/download-link", handler.CreateDownloadLink)
	issueResponse := httptest.NewRecorder()
	issueRouter.ServeHTTP(
		issueResponse,
		httptest.NewRequest(http.MethodPost, "/knowledge/knowledge-1/download-link", nil),
	)
	if issueResponse.Code != http.StatusOK {
		t.Fatalf("issue status = %d, body = %s", issueResponse.Code, issueResponse.Body.String())
	}

	downloadURL, _, err := service.Issue(context.Background(), "knowledge-1")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	parsed, err := url.Parse(downloadURL)
	if err != nil {
		t.Fatalf("parse download URL: %v", err)
	}

	downloadRouter := gin.New()
	downloadRouter.GET(downloadPath, handler.Download)
	response := httptest.NewRecorder()
	downloadRouter.ServeHTTP(response, httptest.NewRequest(http.MethodGet, parsed.RequestURI(), nil))

	if response.Code != http.StatusOK {
		t.Fatalf("download status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Body.String(); got != "pdf-body" {
		t.Fatalf("download body = %q", got)
	}
	if files.openedTenant != 10023 {
		t.Fatalf("opened tenant = %d, want 10023", files.openedTenant)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := response.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestSignedDownloadRejectsTamperingAndExpiry(t *testing.T) {
	now := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)
	files := &fakeKnowledgeFiles{
		record: &types.Knowledge{ID: "knowledge-1", TenantID: 10023, FileName: "a.txt"},
		body:   "body",
	}
	service := NewService(files, Config{
		SigningKey: []byte("0123456789abcdef0123456789abcdef"),
		TTL:        time.Minute,
	})
	service.now = func() time.Time { return now }
	rawURL, _, err := service.Issue(context.Background(), "knowledge-1")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	parsed, _ := url.Parse(rawURL)

	tampered := parsed.Query()
	tampered.Set("tenant_id", "10024")
	if _, err := service.Resolve(context.Background(), tampered); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("tampered Resolve() error = %v, want ErrInvalidTicket", err)
	}

	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := service.Resolve(context.Background(), parsed.Query()); !errors.Is(err, ErrExpiredTicket) {
		t.Fatalf("expired Resolve() error = %v, want ErrExpiredTicket", err)
	}
}

package mobiledocument

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

type fakeArtifactFiles struct {
	record       *ArtifactFile
	body         string
	openedID     string
	openedTenant uint64
	openedUser   string
}

func (f *fakeArtifactFiles) GetArtifact(
	_ context.Context,
	id string,
	tenantID uint64,
	userID string,
) (*ArtifactFile, error) {
	if f.record == nil || f.record.ID != id || f.record.TenantID != tenantID || f.record.UserID != userID {
		return nil, ErrArtifactUnavailable
	}
	copyRecord := *f.record
	return &copyRecord, nil
}

func (f *fakeArtifactFiles) OpenArtifact(_ context.Context, file *ArtifactFile) (io.ReadCloser, error) {
	if f.record == nil || file == nil || file.ID != f.record.ID {
		return nil, ErrArtifactUnavailable
	}
	f.openedID = file.ID
	f.openedTenant = file.TenantID
	f.openedUser = file.UserID
	return io.NopCloser(strings.NewReader(f.body)), nil
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
	service := NewService(files, nil, Config{
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
	service := NewService(files, nil, Config{
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

func TestSignedArtifactDownloadUsesSameNativeAttachmentContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	artifacts := &fakeArtifactFiles{
		record: &ArtifactFile{
			ID:           "artifact-1",
			TenantID:     10023,
			UserID:       "user-1",
			StorageState: artifactStorageStateReady,
			FileName:     "企业报告.docx",
			FileSize:     int64(len("docx-body")),
			ContentType:  "application/octet-stream",
		},
		body: "docx-body",
	}
	service := NewService(nil, artifacts, Config{
		SigningKey: []byte("0123456789abcdef0123456789abcdef"),
		TTL:        2 * time.Minute,
	})
	service.now = func() time.Time { return now }
	handler := NewHandler(service)

	issueRouter := gin.New()
	issueRouter.Use(func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(10023))
		ctx = context.WithValue(ctx, types.UserIDContextKey, "user-1")
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	issueRouter.POST("/artifacts/:artifact_id/download-link", handler.CreateArtifactDownloadLink)
	issueResponse := httptest.NewRecorder()
	issueRouter.ServeHTTP(
		issueResponse,
		httptest.NewRequest(http.MethodPost, "/artifacts/artifact-1/download-link", nil),
	)
	if issueResponse.Code != http.StatusOK {
		t.Fatalf("issue status = %d, body = %s", issueResponse.Code, issueResponse.Body.String())
	}
	var issued struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(issueResponse.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode issue response: %v", err)
	}
	parsed, err := url.Parse(issued.Data.URL)
	if err != nil || parsed.Path != artifactDownloadPath {
		t.Fatalf("issued artifact URL = %q, parse error = %v", issued.Data.URL, err)
	}

	downloadRouter := gin.New()
	downloadRouter.GET(artifactDownloadPath, handler.DownloadArtifact)
	response := httptest.NewRecorder()
	downloadRouter.ServeHTTP(response, httptest.NewRequest(http.MethodGet, parsed.RequestURI(), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("download status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Body.String(); got != "docx-body" {
		t.Fatalf("download body = %q", got)
	}
	if artifacts.openedID != "artifact-1" || artifacts.openedTenant != 10023 || artifacts.openedUser != "user-1" {
		t.Fatalf(
			"opened artifact = (%q, %d, %q)",
			artifacts.openedID,
			artifacts.openedTenant,
			artifacts.openedUser,
		)
	}
	if got := response.Header().Get("Content-Type"); got != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len("docx-body")) {
		t.Fatalf("Content-Length = %q", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition = %q", got)
	}
}

func TestSignedArtifactDownloadRejectsWrongOwnerTamperingAndExpiry(t *testing.T) {
	now := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	artifacts := &fakeArtifactFiles{
		record: &ArtifactFile{
			ID:           "artifact-1",
			TenantID:     10023,
			UserID:       "user-1",
			StorageState: artifactStorageStateReady,
			FileName:     "a.txt",
		},
		body: "body",
	}
	service := NewService(nil, artifacts, Config{
		SigningKey: []byte("0123456789abcdef0123456789abcdef"),
		TTL:        time.Minute,
	})
	service.now = func() time.Time { return now }
	if _, _, err := service.IssueArtifact(context.Background(), "artifact-1", 10023, "other-user"); !errors.Is(err, ErrArtifactUnavailable) {
		t.Fatalf("wrong-owner IssueArtifact() error = %v", err)
	}

	rawURL, _, err := service.IssueArtifact(context.Background(), "artifact-1", 10023, "user-1")
	if err != nil {
		t.Fatalf("IssueArtifact() error = %v", err)
	}
	parsed, _ := url.Parse(rawURL)
	tampered := parsed.Query()
	tampered.Set("user_id", "user-2")
	if _, err := service.ResolveArtifact(context.Background(), tampered); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("tampered ResolveArtifact() error = %v, want ErrInvalidTicket", err)
	}

	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := service.ResolveArtifact(context.Background(), parsed.Query()); !errors.Is(err, ErrExpiredTicket) {
		t.Fatalf("expired ResolveArtifact() error = %v, want ErrExpiredTicket", err)
	}
}

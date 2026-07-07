package iam

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSSOEntryRedirectURIUsesForwardedOrigin(t *testing.T) {
	router := newSSOEntryTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/custom/iam/sso/entry", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "knora.moutai.com.cn")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusFound, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse location %q: %v", location, err)
	}
	if got, want := parsed.Scheme+"://"+parsed.Host+parsed.Path, "https://iam.example.com.cn/idp/authCenter/authenticate"; got != want {
		t.Fatalf("authorization endpoint = %q, want %q", got, want)
	}
	if got, want := parsed.Query().Get("redirect_uri"), "https://knora.moutai.com.cn/api/v1/custom/iam/sso/callback"; got != want {
		t.Fatalf("redirect_uri = %q, want %q", got, want)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("state is empty")
	}
	payload, err := decodeSSOState(state)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if got, want := payload.FrontendRedirect, "https://knora.moutai.com.cn/"; got != want {
		t.Fatalf("frontend_redirect = %q, want %q", got, want)
	}
}

func TestSSOEntryStoresLocalFrontendRedirect(t *testing.T) {
	router := newSSOEntryTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/v1/custom/iam/sso/entry", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusFound, rec.Body.String())
	}
	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if got, want := parsed.Query().Get("redirect_uri"), "http://localhost:8080/api/v1/custom/iam/sso/callback"; got != want {
		t.Fatalf("redirect_uri = %q, want %q", got, want)
	}
	payload, err := decodeSSOState(parsed.Query().Get("state"))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if got, want := payload.FrontendRedirect, "http://localhost:5177/"; got != want {
		t.Fatalf("frontend_redirect = %q, want %q", got, want)
	}
	if got, want := ssoBrowserRedirect(payload.FrontendRedirect, url.Values{"oidc_result": {"payload"}}), "http://localhost:5177/#oidc_result=payload"; got != want {
		t.Fatalf("browser redirect = %q, want %q", got, want)
	}
}

func TestSSOEntryStoresMobileFrontendRedirect(t *testing.T) {
	router := newSSOEntryTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/custom/iam/sso/entry?client=mobile", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "knora.moutai.com.cn")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusFound, rec.Body.String())
	}
	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if got, want := parsed.Query().Get("redirect_uri"), "https://knora.moutai.com.cn/api/v1/custom/iam/sso/callback"; got != want {
		t.Fatalf("redirect_uri = %q, want %q", got, want)
	}
	payload, err := decodeSSOState(parsed.Query().Get("state"))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if got, want := payload.FrontendRedirect, "https://knora.moutai.com.cn/mobile/"; got != want {
		t.Fatalf("frontend_redirect = %q, want %q", got, want)
	}
	if got, want := ssoBrowserRedirect(payload.FrontendRedirect, url.Values{"oidc_result": {"payload"}}), "https://knora.moutai.com.cn/mobile/#oidc_result=payload"; got != want {
		t.Fatalf("browser redirect = %q, want %q", got, want)
	}
}

func TestSSOEntryStoresLocalMobileFrontendRedirect(t *testing.T) {
	router := newSSOEntryTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/v1/custom/iam/sso/entry?client=mobile", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusFound, rec.Body.String())
	}
	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if got, want := parsed.Query().Get("redirect_uri"), "http://localhost:8080/api/v1/custom/iam/sso/callback"; got != want {
		t.Fatalf("redirect_uri = %q, want %q", got, want)
	}
	payload, err := decodeSSOState(parsed.Query().Get("state"))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if got, want := payload.FrontendRedirect, "http://localhost:8080/mobile/"; got != want {
		t.Fatalf("frontend_redirect = %q, want %q", got, want)
	}
	if got, want := ssoBrowserRedirect(payload.FrontendRedirect, url.Values{"oidc_result": {"payload"}}), "http://localhost:8080/mobile/#oidc_result=payload"; got != want {
		t.Fatalf("browser redirect = %q, want %q", got, want)
	}
}

func TestSSOCallbackURLFromRequestDefaultsToHTTPSForPublicHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/custom/iam/sso/entry", nil)
	req.Host = "knora.moutai.com.cn"

	got, err := ssoCallbackURLFromRequest(req)
	if err != nil {
		t.Fatalf("ssoCallbackURLFromRequest returned error: %v", err)
	}
	want := "https://knora.moutai.com.cn/api/v1/custom/iam/sso/callback"
	if got != want {
		t.Fatalf("callback URL = %q, want %q", got, want)
	}
}

func newSSOEntryTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&SyncSetting{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&SyncSetting{
		ID:                1,
		BaseURL:           "https://iam.example.com.cn",
		LoginClientID:     "client-id",
		LoginClientSecret: "client-secret",
	}).Error; err != nil {
		t.Fatalf("create setting: %v", err)
	}
	router := gin.New()
	handler := NewHandler(NewService(db, nil), nil)
	router.GET("/api/v1/custom/iam/sso/entry", handler.SSOEntry)
	return router
}

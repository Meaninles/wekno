package iam

import (
	"encoding/json"
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

func TestSSOEntryStoresExactCitationDeepLink(t *testing.T) {
	router := newSSOEntryTestRouterWithPublicOrigin(t, "https://knora.moutai.com.cn")
	returnTo := "/platform/knowledge-bases/kb-1?knowledge_id=doc-1&chunk_id=chunk-1"
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/custom/iam/sso/entry?return_to="+url.QueryEscape(returnTo),
		nil,
	)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusFound, rec.Body.String())
	}
	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	payload, err := decodeSSOState(parsed.Query().Get("state"))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	want := "https://knora.moutai.com.cn" + returnTo
	if payload.FrontendRedirect != want {
		t.Fatalf("frontend_redirect = %q, want %q", payload.FrontendRedirect, want)
	}
	if got := ssoBrowserRedirect(payload.FrontendRedirect, url.Values{"oidc_result": {"payload"}}); got != want+"#oidc_result=payload" {
		t.Fatalf("browser redirect = %q, want citation deep link", got)
	}
}

func TestSSOEntryStoresExactMobileCitationDeepLink(t *testing.T) {
	router := newSSOEntryTestRouterWithPublicOrigin(t, "https://knora.moutai.com.cn")
	returnTo := "/mobile/reference?type=wiki&knowledge_base_id=kb-1&slug=concept%2Freserve"
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/custom/iam/sso/entry?client=mobile&return_to="+url.QueryEscape(returnTo),
		nil,
	)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusFound, rec.Body.String())
	}
	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	payload, err := decodeSSOState(parsed.Query().Get("state"))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	want := "https://knora.moutai.com.cn" + returnTo
	if payload.FrontendRedirect != want {
		t.Fatalf("frontend_redirect = %q, want %q", payload.FrontendRedirect, want)
	}
}

func TestNormalizeSSOReturnPathRejectsOpenRedirects(t *testing.T) {
	for _, value := range []string{
		"https://evil.example/steal",
		"//evil.example/steal",
		"/api/v1/users",
		"/platform/knowledge-bases/kb#oidc_result=fake",
	} {
		if got, err := normalizeSSOReturnPath(value); err == nil || got != "" {
			t.Fatalf("normalizeSSOReturnPath(%q) = %q, %v; want rejection", value, got, err)
		}
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

func TestSSOEntryConfiguredPublicOriginOverridesForwardedHTTP(t *testing.T) {
	router := newSSOEntryTestRouterWithPublicOrigin(t, "https://knora.moutai.com.cn")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/custom/iam/sso/entry", nil)
	req.Host = "app:8080"
	req.Header.Set("X-Forwarded-Host", "knora.moutai.com.cn")
	req.Header.Set("X-Forwarded-Proto", "http")
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
	if got, want := payload.FrontendRedirect, "https://knora.moutai.com.cn/"; got != want {
		t.Fatalf("frontend_redirect = %q, want %q", got, want)
	}
}

func TestSSOEntryConfiguredPublicOriginKeepsMobileRedirectHTTPS(t *testing.T) {
	router := newSSOEntryTestRouterWithPublicOrigin(t, "https://knora.moutai.com.cn")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/custom/iam/sso/entry?client=mobile", nil)
	req.Header.Set("X-Forwarded-Proto", "http")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	payload, err := decodeSSOState(parsed.Query().Get("state"))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if got, want := payload.FrontendRedirect, "https://knora.moutai.com.cn/mobile/"; got != want {
		t.Fatalf("frontend_redirect = %q, want %q", got, want)
	}
}

func TestSSOAuthURLConfiguredPublicOriginRejectsDifferentRedirectURI(t *testing.T) {
	router := newSSOEntryTestRouterWithPublicOrigin(t, "https://knora.moutai.com.cn")
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/custom/iam/sso/url?redirect_uri="+url.QueryEscape("http://knora.moutai.com.cn/api/v1/custom/iam/sso/callback"),
		nil,
	)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestSSOAuthURLConfiguredPublicOriginSuppliesCanonicalRedirectURI(t *testing.T) {
	router := newSSOEntryTestRouterWithPublicOrigin(t, "https://knora.moutai.com.cn")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/custom/iam/sso/url", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var response SSOAuthURLResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	parsed, err := url.Parse(response.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if got, want := parsed.Query().Get("redirect_uri"), "https://knora.moutai.com.cn/api/v1/custom/iam/sso/callback"; got != want {
		t.Fatalf("redirect_uri = %q, want %q", got, want)
	}
}

func TestSSOCallbackURLFromRequestDefaultsToHTTPSForPublicHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/custom/iam/sso/entry", nil)
	req.Host = "knora.moutai.com.cn"

	got, err := ssoCallbackURLFromRequest(req, "")
	if err != nil {
		t.Fatalf("ssoCallbackURLFromRequest returned error: %v", err)
	}
	want := "https://knora.moutai.com.cn/api/v1/custom/iam/sso/callback"
	if got != want {
		t.Fatalf("callback URL = %q, want %q", got, want)
	}
}

func newSSOEntryTestRouter(t *testing.T) *gin.Engine {
	return newSSOEntryTestRouterWithPublicOrigin(t, "")
}

func newSSOEntryTestRouterWithPublicOrigin(t *testing.T, publicOrigin string) *gin.Engine {
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
	handler := NewHandler(NewService(db, nil), nil, publicOrigin)
	router.GET("/api/v1/custom/iam/sso/entry", handler.SSOEntry)
	router.GET("/api/v1/custom/iam/sso/url", handler.GetSSOAuthorizationURL)
	return router
}

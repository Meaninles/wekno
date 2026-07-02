package iam

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestExchangeSSOTokenExtractsNestedAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/idp/api/v3/oauth2/token" {
			t.Fatalf("path = %q, want /idp/api/v3/oauth2/token", r.URL.Path)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("client-id:client-secret"))
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Fatalf("Authorization = %q, want %q", got, wantAuth)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm returned error: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "authorization_code" {
			t.Fatalf("grant_type = %q, want authorization_code", got)
		}
		if got := r.Form.Get("code"); got != "auth-code" {
			t.Fatalf("code = %q, want auth-code", got)
		}
		if got := r.Form.Get("client_id"); got != "" {
			t.Fatalf("client_id must be sent via Basic auth, got form value %q", got)
		}
		if got := r.Form.Get("client_secret"); got != "" {
			t.Fatalf("client_secret must be sent via Basic auth, got form value %q", got)
		}
		if got := r.Form.Get("redirect_uri"); got != "" {
			t.Fatalf("redirect_uri = %q, want empty per BambooCloud OAuth2 token API", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":"0","data":{"access_token":"nested-token","expires_in":7200}}`))
	}))
	defer server.Close()

	service := &Service{httpClient: server.Client()}
	tokenResp, err := service.exchangeSSOToken(context.Background(), &SyncSetting{
		BaseURL:           server.URL,
		LoginClientID:     "client-id",
		LoginClientSecret: "client-secret",
	}, "auth-code", "http://app.example.com/api/v1/custom/iam/sso/callback")
	if err != nil {
		t.Fatalf("exchangeSSOToken returned error: %v", err)
	}
	if tokenResp.AccessToken != "nested-token" {
		t.Fatalf("AccessToken = %q, want nested-token", tokenResp.AccessToken)
	}
}

func TestSSOTokenExtractionFindsWrappedAccessToken(t *testing.T) {
	token := ssoAccessTokenFromResponse(map[string]any{
		"code": "200",
		"datas": map[string]any{
			"account": map[string]any{
				"ACCESS-TOKEN": "wrapped-token",
			},
		},
	})

	if token != "wrapped-token" {
		t.Fatalf("token = %q, want wrapped-token", token)
	}
}

func TestExchangeSSOTokenRejectsJSONWithoutAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":"0","data":{"expires_in":7200}}`))
	}))
	defer server.Close()

	service := &Service{httpClient: server.Client()}
	tokenResp, err := service.exchangeSSOToken(context.Background(), &SyncSetting{
		BaseURL:           server.URL,
		LoginClientID:     "client-id",
		LoginClientSecret: "client-secret",
	}, "auth-code", "http://app.example.com/api/v1/custom/iam/sso/callback")
	if err == nil {
		t.Fatalf("exchangeSSOToken returned token response %#v, want error", tokenResp)
	}
	if !strings.Contains(err.Error(), "missing access token") {
		t.Fatalf("error = %q, want missing access token", err.Error())
	}
}

func TestFetchSSOUserInfoUsesOAuth2V3EndpointAndBearerHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/idp/api/v3/oauth2/userInfo" {
			t.Fatalf("path = %q, want /idp/api/v3/oauth2/userInfo", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q, want Bearer access-token", got)
		}
		if got := r.URL.Query().Get("access_token"); got != "" {
			t.Fatalf("access_token query = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userId":"u-001","userName":"alice","spRoleList":["alice"]}`))
	}))
	defer server.Close()

	service := &Service{httpClient: server.Client()}
	claims, err := service.fetchSSOUserInfo(context.Background(), &SyncSetting{
		BaseURL: server.URL,
	}, "access-token")
	if err != nil {
		t.Fatalf("fetchSSOUserInfo returned error: %v", err)
	}
	if got := claims["userName"]; got != "alice" {
		t.Fatalf("userName = %v, want alice", got)
	}
}

func TestExtractItemsAcceptsSingleIAMUserObject(t *testing.T) {
	raw := []byte(`{
		"userId":"u-001",
		"organizationId":"org-001",
		"username":"alice",
		"fullname":"Alice",
		"isDisabled":false,
		"_ID":"row-001"
	}`)

	items, hasMore, err := extractItems(raw, 200)
	if err != nil {
		t.Fatalf("extractItems returned error: %v", err)
	}
	if hasMore {
		t.Fatal("single object response must not request another page")
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if got := iamAccountNameFromItem(items[0]); got != "alice" {
		t.Fatalf("iamAccountNameFromItem = %q, want alice", got)
	}
}

func TestExternalUserFromClaimsUsesAccountAsUsername(t *testing.T) {
	ext := externalUserFromClaims(map[string]any{
		"uid":      "alice",
		"fullname": "Alice",
	})

	if ext.ExternalID != "alice" {
		t.Fatalf("ExternalID = %q, want alice", ext.ExternalID)
	}
	if ext.Username != "alice" {
		t.Fatalf("Username = %q, want alice", ext.Username)
	}
}

func TestListSpaceMemberCandidateOrganizationsAggregatesDescendantUserCounts(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalOrganization{}, &ExternalUser{}, &types.User{}, &types.Tenant{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	orgs := []ExternalOrganization{
		{ExternalID: "root", Name: "总部"},
		{ExternalID: "child", Name: "分公司", ParentExternalID: "root"},
		{ExternalID: "leaf", Name: "项目部", ParentExternalID: "child"},
	}
	if err := db.Create(&orgs).Error; err != nil {
		t.Fatalf("create organizations: %v", err)
	}
	tenants := []types.Tenant{
		{ID: 1, Name: "tenant-root"},
		{ID: 2, Name: "tenant-child"},
		{ID: 3, Name: "tenant-leaf"},
	}
	if err := db.Create(&tenants).Error; err != nil {
		t.Fatalf("create tenants: %v", err)
	}
	users := []types.User{
		{ID: "u-root", Username: "root-user", PasswordHash: "x", TenantID: 1, IsActive: true},
		{ID: "u-child", Username: "child-user", PasswordHash: "x", TenantID: 2, IsActive: true},
		{ID: "u-leaf", Username: "leaf-user", PasswordHash: "x", TenantID: 3, IsActive: true},
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatalf("create users: %v", err)
	}
	externalUsers := []ExternalUser{
		{ExternalID: "root-user", Username: "root-user", DisplayName: "Root", OrganizationExternalID: "root", WeKnoraUserID: "u-root"},
		{ExternalID: "child-user", Username: "child-user", DisplayName: "Child", OrganizationExternalID: "child", WeKnoraUserID: "u-child"},
		{ExternalID: "leaf-user", Username: "leaf-user", DisplayName: "Leaf", OrganizationExternalID: "leaf", WeKnoraUserID: "u-leaf"},
	}
	if err := db.Create(&externalUsers).Error; err != nil {
		t.Fatalf("create external users: %v", err)
	}

	service := NewService(db, nil)
	rows, err := service.ListSpaceMemberCandidateOrganizations(context.Background(), "space-1", "", 100)
	if err != nil {
		t.Fatalf("ListSpaceMemberCandidateOrganizations returned error: %v", err)
	}

	countByOrg := map[string]int64{}
	for _, row := range rows {
		countByOrg[row.ExternalID] = row.UserCount
	}
	if countByOrg["root"] != 3 {
		t.Fatalf("root user_count = %d, want 3", countByOrg["root"])
	}
	if countByOrg["child"] != 2 {
		t.Fatalf("child user_count = %d, want 2", countByOrg["child"])
	}
	if countByOrg["leaf"] != 1 {
		t.Fatalf("leaf user_count = %d, want 1", countByOrg["leaf"])
	}
}

func TestListSpaceMemberCandidateUsersIncludesDescendantOrganizations(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalOrganization{}, &ExternalUser{}, &types.User{}, &types.Tenant{}, &types.OrganizationTenantMember{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	orgs := []ExternalOrganization{
		{ExternalID: "root", Name: "总部"},
		{ExternalID: "child", Name: "分公司", ParentExternalID: "root"},
		{ExternalID: "leaf", Name: "项目部", ParentExternalID: "child"},
	}
	if err := db.Create(&orgs).Error; err != nil {
		t.Fatalf("create organizations: %v", err)
	}
	tenants := []types.Tenant{
		{ID: 1, Name: "tenant-root"},
		{ID: 2, Name: "tenant-child"},
		{ID: 3, Name: "tenant-leaf"},
	}
	if err := db.Create(&tenants).Error; err != nil {
		t.Fatalf("create tenants: %v", err)
	}
	users := []types.User{
		{ID: "u-root", Username: "root-user", PasswordHash: "x", TenantID: 1, IsActive: true},
		{ID: "u-child", Username: "child-user", PasswordHash: "x", TenantID: 2, IsActive: true},
		{ID: "u-leaf", Username: "leaf-user", PasswordHash: "x", TenantID: 3, IsActive: true},
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatalf("create users: %v", err)
	}
	externalUsers := []ExternalUser{
		{ExternalID: "root-user", Username: "root-user", DisplayName: "Root", OrganizationExternalID: "root", WeKnoraUserID: "u-root"},
		{ExternalID: "child-user", Username: "child-user", DisplayName: "Child", OrganizationExternalID: "child", WeKnoraUserID: "u-child"},
		{ExternalID: "leaf-user", Username: "leaf-user", DisplayName: "Leaf", OrganizationExternalID: "leaf", WeKnoraUserID: "u-leaf"},
	}
	if err := db.Create(&externalUsers).Error; err != nil {
		t.Fatalf("create external users: %v", err)
	}

	service := NewService(db, nil)
	rows, err := service.ListSpaceMemberCandidateUsers(context.Background(), "space-1", "", []string{"root"}, 100)
	if err != nil {
		t.Fatalf("ListSpaceMemberCandidateUsers returned error: %v", err)
	}

	got := map[string]bool{}
	for _, row := range rows {
		got[row.UserID] = true
	}
	for _, want := range []string{"u-root", "u-child", "u-leaf"} {
		if !got[want] {
			t.Fatalf("candidate users = %#v, missing %s", got, want)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
}

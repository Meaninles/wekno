package iam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func TestScopedOrganizationSyncUsesIAMFilters(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalOrganization{}, &ExternalUser{}, &PendingSpaceMemberGrant{}, &types.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	orgs := []map[string]any{
		{"_ID": "root", "organizationId": "root", "name": "总部"},
		{"_ID": "child-a", "organizationId": "child-a", "parentId": "root", "name": "一部"},
		{"_ID": "child-b", "organizationId": "child-b", "parentId": "root", "name": "二部"},
		{"_ID": "leaf", "organizationId": "leaf", "parentId": "child-a", "name": "项目组"},
	}
	users := []map[string]any{
		{"_AID": "u-root", "username": "root-user", "fullname": "Root", "organizationId": "root"},
		{"_AID": "u-child-a", "username": "child-a-user", "fullname": "Child A", "organizationId": "child-a"},
		{"_AID": "u-child-b", "username": "child-b-user", "fullname": "Child B", "organizationId": "child-b"},
		{"_AID": "u-leaf", "username": "leaf-user", "fullname": "Leaf", "organizationId": "leaf"},
	}

	var accountFilters []map[string]any
	var organizationFilters []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bim-server/ext/rest/integration/ExtApiIngtAuthService/login":
			_, _ = w.Write([]byte(`"access-token"`))
		case "/bim-server/ext/rest/integration/ExtApiIngtAuthService/logout":
			w.WriteHeader(http.StatusOK)
		case "/bim-server/ext/rest/integration/ExtApiIngtTargetOrganizationService/findBy":
			filters := readTestFilters(t, r)
			organizationFilters = append(organizationFilters, filters)
			if len(filters) == 0 {
				http.Error(w, "organization request must be filtered", http.StatusInternalServerError)
				return
			}
			writeTestItems(t, w, matchingTestOrganizations(orgs, filters))
		case "/bim-server/ext/rest/integration/ExtApiIngtTargetAccountService/findBy":
			filters := readTestFilters(t, r)
			accountFilters = append(accountFilters, filters)
			if len(filters) == 0 {
				http.Error(w, "account request must be filtered", http.StatusInternalServerError)
				return
			}
			writeTestItems(t, w, matchingTestUsers(users, filters))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := NewService(db, nil)
	service.httpClient = server.Client()
	result, err := service.syncOnce(context.Background(), &SyncSetting{
		BaseURL:          server.URL,
		SyncClientID:     "client-id",
		SyncClientSecret: "client-secret",
	}, SyncScope{OrganizationExternalID: "root"})
	if err != nil {
		t.Fatalf("syncOnce returned error: %v", err)
	}
	if result.OrgCount != 4 || result.UserCount != 4 || result.CreatedUsers != 4 {
		t.Fatalf("result = %#v, want 4 orgs and 4 created users", result)
	}
	if len(accountFilters) == 0 {
		t.Fatal("account endpoint was not called")
	}
	for _, filters := range accountFilters {
		if _, ok := filters["organizationId_in"]; !ok {
			t.Fatalf("account filters = %#v, want organizationId_in", filters)
		}
	}
	for _, filters := range organizationFilters {
		if _, ok := filters["organizationId_eq"]; ok {
			continue
		}
		if _, ok := filters["parentId_in"]; ok {
			continue
		}
		t.Fatalf("organization filters = %#v, want organizationId_eq or parentId_in", filters)
	}

	var orgCount int64
	if err := db.Model(&ExternalOrganization{}).Count(&orgCount).Error; err != nil {
		t.Fatalf("count organizations: %v", err)
	}
	if orgCount != 4 {
		t.Fatalf("stored organization count = %d, want 4", orgCount)
	}
	var userCount int64
	if err := db.Model(&ExternalUser{}).Count(&userCount).Error; err != nil {
		t.Fatalf("count users: %v", err)
	}
	if userCount != 4 {
		t.Fatalf("stored user count = %d, want 4", userCount)
	}
}

func readTestFilters(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body struct {
		Filters map[string]any `json:"filters"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body.Filters
}

func writeTestItems(t *testing.T, w http.ResponseWriter, items []map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": items}); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func matchingTestOrganizations(orgs []map[string]any, filters map[string]any) []map[string]any {
	if want := stringFilterValue(filters["organizationId_eq"]); want != "" {
		return filterTestItems(orgs, "organizationId", map[string]bool{want: true})
	}
	return filterTestItems(orgs, "parentId", stringFilterSet(filters["parentId_in"]))
}

func matchingTestUsers(users []map[string]any, filters map[string]any) []map[string]any {
	return filterTestItems(users, "organizationId", stringFilterSet(filters["organizationId_in"]))
}

func filterTestItems(items []map[string]any, key string, allowed map[string]bool) []map[string]any {
	if len(allowed) == 0 {
		return nil
	}
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if allowed[firstString(item, key)] {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func stringFilterSet(value any) map[string]bool {
	values := map[string]bool{}
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
				values[s] = true
			}
		}
	case []string:
		for _, item := range v {
			if s := strings.TrimSpace(item); s != "" {
				values[s] = true
			}
		}
	}
	return values
}

func stringFilterValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
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

func TestListSpaceMemberCandidateOrganizationsReturnsDirectChildren(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalOrganization{}, &ExternalUser{}, &PendingSpaceMemberGrant{}, &types.User{}, &types.Tenant{}, &types.OrganizationTenantMember{}); err != nil {
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
	if err := db.Create(&types.OrganizationTenantMember{
		ID:             "member-leaf",
		OrganizationID: "space-1",
		TenantID:       3,
		Role:           types.OrgRoleViewer,
	}).Error; err != nil {
		t.Fatalf("create existing space member: %v", err)
	}

	service := NewService(db, nil)
	if err := service.refreshOrganizationSubtreeUserCounts(context.Background()); err != nil {
		t.Fatalf("refreshOrganizationSubtreeUserCounts returned error: %v", err)
	}
	rows, err := service.ListSpaceMemberCandidateOrganizations(context.Background(), "space-1", "", "", 100)
	if err != nil {
		t.Fatalf("ListSpaceMemberCandidateOrganizations returned error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(root rows) = %d, want 1", len(rows))
	}
	if rows[0].ExternalID != "root" {
		t.Fatalf("root row external_id = %q, want root", rows[0].ExternalID)
	}
	if rows[0].UserCount != 3 {
		t.Fatalf("root subtree user_count = %d, want 3", rows[0].UserCount)
	}
	if !rows[0].HasChildren {
		t.Fatal("root has_children = false, want true")
	}

	childRows, err := service.ListSpaceMemberCandidateOrganizations(context.Background(), "space-1", "root", "", 100)
	if err != nil {
		t.Fatalf("ListSpaceMemberCandidateOrganizations child returned error: %v", err)
	}
	if len(childRows) != 1 {
		t.Fatalf("len(child rows) = %d, want 1", len(childRows))
	}
	if childRows[0].ExternalID != "child" {
		t.Fatalf("child row external_id = %q, want child", childRows[0].ExternalID)
	}
	if childRows[0].UserCount != 2 {
		t.Fatalf("child subtree user_count = %d, want 2", childRows[0].UserCount)
	}
	if !childRows[0].HasChildren {
		t.Fatal("child has_children = false, want true")
	}

	leafRows, err := service.ListSpaceMemberCandidateOrganizations(context.Background(), "space-1", "child", "", 100)
	if err != nil {
		t.Fatalf("ListSpaceMemberCandidateOrganizations leaf returned error: %v", err)
	}
	if len(leafRows) != 1 {
		t.Fatalf("len(leaf rows) = %d, want 1", len(leafRows))
	}
	if leafRows[0].ExternalID != "leaf" {
		t.Fatalf("leaf row external_id = %q, want leaf", leafRows[0].ExternalID)
	}
	if leafRows[0].UserCount != 1 {
		t.Fatalf("leaf direct user_count = %d, want 1", leafRows[0].UserCount)
	}
	if leafRows[0].HasChildren {
		t.Fatal("leaf has_children = true, want false")
	}

	leafWithUsers, err := service.ListSpaceMemberCandidateOrganizations(context.Background(), "space-1", "leaf", "", 100, true)
	if err != nil {
		t.Fatalf("ListSpaceMemberCandidateOrganizations leaf users returned error: %v", err)
	}
	if len(leafWithUsers) != 1 {
		t.Fatalf("len(leafWithUsers) = %d, want 1", len(leafWithUsers))
	}
	if leafWithUsers[0].NodeType != "user" || leafWithUsers[0].IAMExternalID != "leaf-user" {
		t.Fatalf("leaf user row = %#v, want iam leaf-user", leafWithUsers[0])
	}
	if !leafWithUsers[0].AlreadySelected || !leafWithUsers[0].SelectionDisabled {
		t.Fatalf("leaf user selection state = selected:%v disabled:%v, want true/true", leafWithUsers[0].AlreadySelected, leafWithUsers[0].SelectionDisabled)
	}
}

func TestListSpaceMemberCandidateUsersIncludesDescendantOrganizations(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalOrganization{}, &ExternalUser{}, &PendingSpaceMemberGrant{}, &types.User{}, &types.Tenant{}, &types.OrganizationTenantMember{}); err != nil {
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
	if err := db.Create(&types.OrganizationTenantMember{
		ID:             "member-child",
		OrganizationID: "space-1",
		TenantID:       2,
		Role:           types.OrgRoleViewer,
	}).Error; err != nil {
		t.Fatalf("create existing space member: %v", err)
	}
	if err := db.Create(&PendingSpaceMemberGrant{
		OrganizationID:    "space-1",
		IAMExternalUserID: "leaf-user",
		Role:              types.OrgRoleViewer,
	}).Error; err != nil {
		t.Fatalf("create pending grant: %v", err)
	}

	service := NewService(db, nil)
	rows, err := service.ListSpaceMemberCandidateUsers(context.Background(), "space-1", "", []string{"root"}, false, 100)
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
	selectedByUserID := map[string]bool{}
	disabledByUserID := map[string]bool{}
	for _, row := range rows {
		selectedByUserID[row.UserID] = row.AlreadySelected
		disabledByUserID[row.UserID] = row.SelectionDisabled
	}
	if !selectedByUserID["u-child"] || !disabledByUserID["u-child"] {
		t.Fatalf("existing member selection state for u-child = selected:%v disabled:%v, want true/true", selectedByUserID["u-child"], disabledByUserID["u-child"])
	}
	if !selectedByUserID["u-leaf"] || !disabledByUserID["u-leaf"] {
		t.Fatalf("pending member selection state for u-leaf = selected:%v disabled:%v, want true/true", selectedByUserID["u-leaf"], disabledByUserID["u-leaf"])
	}

	directRows, err := service.ListSpaceMemberCandidateUsers(context.Background(), "space-1", "", []string{"root"}, true, 100)
	if err != nil {
		t.Fatalf("ListSpaceMemberCandidateUsers direct returned error: %v", err)
	}
	if len(directRows) != 1 {
		t.Fatalf("len(direct rows) = %d, want 1", len(directRows))
	}
	if directRows[0].UserID != "u-root" {
		t.Fatalf("direct user_id = %q, want u-root", directRows[0].UserID)
	}
}

func TestUpsertUsersUpdatesChangedUsefulFields(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalUser{}, &PendingSpaceMemberGrant{}, &types.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&ExternalUser{
		ExternalID:             "alice",
		ExternalAccountID:      "aid-alice",
		Username:               "alice",
		DisplayName:            "alice",
		OrganizationExternalID: "org-old",
		Disabled:               false,
		Raw:                    `{"old":true}`,
	}).Error; err != nil {
		t.Fatalf("create existing external user: %v", err)
	}

	service := NewService(db, nil)
	result := &SyncResult{}
	err = service.upsertUsers(context.Background(), []map[string]any{
		{
			"_AID":           "aid-alice",
			"username":       "alice",
			"fullname":       "Alice New",
			"organizationId": "org-new",
			"isDisabled":     true,
			"password":       "volatile-secret",
			"updateAt":       "2026-07-08 10:00:00",
		},
		{
			"_AID":           "aid-bob",
			"username":       "bob",
			"fullname":       "Bob",
			"organizationId": "org-new",
			"isDisabled":     true,
		},
	}, result)
	if err != nil {
		t.Fatalf("upsertUsers returned error: %v", err)
	}

	if result.CreatedUsers != 1 {
		t.Fatalf("CreatedUsers = %d, want 1", result.CreatedUsers)
	}
	if result.UpdatedUsers != 1 {
		t.Fatalf("UpdatedUsers = %d, want 1", result.UpdatedUsers)
	}
	if result.DisabledUsers != 2 {
		t.Fatalf("DisabledUsers = %d, want 2", result.DisabledUsers)
	}

	var alice ExternalUser
	if err := db.First(&alice, "external_account_id = ?", "aid-alice").Error; err != nil {
		t.Fatalf("load alice: %v", err)
	}
	if alice.ExternalID != "aid-alice" || alice.Username != "alice" || alice.DisplayName != "Alice New" || alice.OrganizationExternalID != "org-new" || !alice.Disabled {
		t.Fatalf("existing alice was not updated from useful fields: %#v", alice)
	}
	if strings.Contains(alice.Raw, "password") || strings.Contains(alice.Raw, "volatile-secret") {
		t.Fatalf("alice raw leaked password: %s", alice.Raw)
	}

	var count int64
	if err := db.Model(&ExternalUser{}).Count(&count).Error; err != nil {
		t.Fatalf("count external users: %v", err)
	}
	if count != 2 {
		t.Fatalf("external user count = %d, want 2", count)
	}
}

func TestUpsertUsersDoesNotUpdateUnchangedUsefulFields(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalUser{}, &PendingSpaceMemberGrant{}, &types.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&ExternalUser{
		ExternalID:             "aid-alice",
		ExternalAccountID:      "aid-alice",
		Username:               "alice",
		DisplayName:            "Alice",
		OrganizationExternalID: "org",
		Disabled:               false,
		ExternalUpdatedAt:      "old-update-time",
		Raw:                    `{"snapshot":"old"}`,
	}).Error; err != nil {
		t.Fatalf("create existing external user: %v", err)
	}

	service := NewService(db, nil)
	result := &SyncResult{}
	err = service.upsertUsers(context.Background(), []map[string]any{
		{
			"_AID":           "aid-alice",
			"username":       "alice",
			"fullname":       "Alice",
			"organizationId": "org",
			"isDisabled":     false,
			"password":       "new-volatile-secret",
			"updateAt":       "new-update-time",
		},
	}, result)
	if err != nil {
		t.Fatalf("upsertUsers returned error: %v", err)
	}
	if result.CreatedUsers != 0 || result.UpdatedUsers != 0 || result.DisabledUsers != 0 {
		t.Fatalf("result = %#v, want no changes", result)
	}

	var alice ExternalUser
	if err := db.First(&alice, "external_id = ?", "aid-alice").Error; err != nil {
		t.Fatalf("load alice: %v", err)
	}
	if alice.ExternalUpdatedAt != "old-update-time" || alice.Raw != `{"snapshot":"old"}` {
		t.Fatalf("unchanged useful fields should not refresh metadata/raw: %#v", alice)
	}
}

func TestUpsertUsersAttachesExistingLocalUserByUsername(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalUser{}, &PendingSpaceMemberGrant{}, &types.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&types.User{
		ID:           "local-user-1",
		Username:     "20203485",
		DisplayName:  "旧姓名",
		PasswordHash: "x",
		IsActive:     true,
	}).Error; err != nil {
		t.Fatalf("create local user: %v", err)
	}

	service := NewService(db, nil)
	result := &SyncResult{}
	if err := service.upsertUsers(context.Background(), []map[string]any{
		{
			"_AID":           "20260522153812555-7AA0-F48B4A4CA",
			"username":       "20203485",
			"fullname":       "吕扬",
			"organizationId": "org-1",
		},
	}, result); err != nil {
		t.Fatalf("upsertUsers returned error: %v", err)
	}

	var ext ExternalUser
	if err := db.First(&ext, "external_id = ?", "20260522153812555-7AA0-F48B4A4CA").Error; err != nil {
		t.Fatalf("load external user: %v", err)
	}
	if ext.WeKnoraUserID != "local-user-1" {
		t.Fatalf("WeKnoraUserID = %q, want local-user-1", ext.WeKnoraUserID)
	}
	if !ext.AccessEnabled {
		t.Fatal("AccessEnabled = false, want true from local user active state")
	}
	if ext.DisplayName != "吕扬" || ext.Username != "20203485" {
		t.Fatalf("external user identity fields = %#v", ext)
	}
}

func TestCleanupMirrorOnlyUsersWithLocalUsersDeletesMirrorRows(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ExternalUser{}, &types.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&types.User{
		ID:           "local-user-1",
		Username:     "alice",
		PasswordHash: "x",
		IsActive:     true,
	}).Error; err != nil {
		t.Fatalf("create local user: %v", err)
	}
	if err := db.Create(&ExternalUser{
		ExternalID:  "old-alice",
		Username:    "alice",
		DisplayName: "Alice",
	}).Error; err != nil {
		t.Fatalf("create mirror user: %v", err)
	}

	service := NewService(db, nil)
	if err := service.cleanupMirrorOnlyUsersWithLocalUsers(context.Background()); err != nil {
		t.Fatalf("cleanupMirrorOnlyUsersWithLocalUsers returned error: %v", err)
	}

	var activeCount int64
	if err := db.Model(&ExternalUser{}).Where("username = ?", "alice").Count(&activeCount).Error; err != nil {
		t.Fatalf("count active mirror rows: %v", err)
	}
	if activeCount != 0 {
		t.Fatalf("active mirror count = %d, want 0", activeCount)
	}
	var allCount int64
	if err := db.Unscoped().Model(&ExternalUser{}).Where("username = ?", "alice").Count(&allCount).Error; err != nil {
		t.Fatalf("count all mirror rows: %v", err)
	}
	if allCount != 0 {
		t.Fatalf("all mirror count = %d, want 0", allCount)
	}
}

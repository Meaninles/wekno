package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newOrganizationMembersPageTestRepository(t *testing.T) *organizationRepository {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	schema := []string{
		`CREATE TABLE tenants (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL
		)`,
		`CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			avatar TEXT NOT NULL DEFAULT '',
			deleted_at DATETIME
		)`,
		`CREATE TABLE organization_tenant_members (
			id TEXT PRIMARY KEY,
			organization_id TEXT NOT NULL,
			tenant_id INTEGER NOT NULL,
			role TEXT NOT NULL,
			representative_user_id TEXT NOT NULL DEFAULT '',
			joined_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
	}
	for _, statement := range schema {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	return &organizationRepository{db: db}
}

func seedOrganizationMembersPageTestData(t *testing.T, repo *organizationRepository) {
	t.Helper()

	tenants := []struct {
		id   uint64
		name string
	}{
		{id: 1, name: "Alpha Space"},
		{id: 2, name: "Beta Space"},
		{id: 3, name: "100% Team"},
		{id: 4, name: "Delta Space"},
		{id: 5, name: "Other Organization"},
	}
	for _, tenant := range tenants {
		if err := repo.db.Exec(`INSERT INTO tenants (id, name) VALUES (?, ?)`, tenant.id, tenant.name).Error; err != nil {
			t.Fatalf("create tenant %d: %v", tenant.id, err)
		}
	}

	users := []struct {
		id          string
		username    string
		displayName string
	}{
		{id: "user-a", username: "alice", displayName: "Alice"},
		{id: "user-b", username: "bob", displayName: "Display Target"},
		{id: "user-c", username: "carol", displayName: "Carol"},
		{id: "user-d", username: "dave", displayName: "Dave"},
		{id: "user-e", username: "eve", displayName: "Eve"},
	}
	for _, user := range users {
		if err := repo.db.Exec(
			`INSERT INTO users (id, username, display_name, avatar) VALUES (?, ?, ?, '')`,
			user.id, user.username, user.displayName,
		).Error; err != nil {
			t.Fatalf("create user %s: %v", user.id, err)
		}
	}

	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	members := []types.OrganizationTenantMember{
		{ID: "member-a", OrganizationID: "org-a", TenantID: 1, Role: types.OrgRoleAdmin, RepresentativeUserID: "user-a", CreatedAt: base, UpdatedAt: base},
		{ID: "member-b", OrganizationID: "org-a", TenantID: 2, Role: types.OrgRoleViewer, RepresentativeUserID: "user-b", CreatedAt: base.Add(time.Minute), UpdatedAt: base},
		{ID: "member-c", OrganizationID: "org-a", TenantID: 3, Role: types.OrgRoleViewer, RepresentativeUserID: "user-c", CreatedAt: base.Add(2 * time.Minute), UpdatedAt: base},
		{ID: "member-d", OrganizationID: "org-a", TenantID: 4, Role: types.OrgRoleViewer, RepresentativeUserID: "user-d", CreatedAt: base.Add(3 * time.Minute), UpdatedAt: base},
		{ID: "member-e", OrganizationID: "org-b", TenantID: 5, Role: types.OrgRoleViewer, RepresentativeUserID: "user-e", CreatedAt: base.Add(4 * time.Minute), UpdatedAt: base},
	}
	for _, member := range members {
		if err := repo.db.Create(&member).Error; err != nil {
			t.Fatalf("create member %s: %v", member.ID, err)
		}
	}
}

func TestOrganizationRepositoryListTenantMembersPage(t *testing.T) {
	repo := newOrganizationMembersPageTestRepository(t)
	seedOrganizationMembersPageTestData(t, repo)

	members, total, err := repo.ListTenantMembersPage(context.Background(), "org-a", "", 1, 2)
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4", total)
	}
	if len(members) != 2 || members[0].ID != "member-b" || members[1].ID != "member-c" {
		t.Fatalf("unexpected stable page: %#v", members)
	}
	if members[0].RepresentativeUser == nil || members[0].RepresentativeUser.DisplayName != "Display Target" {
		t.Fatalf("representative user was not preloaded: %#v", members[0].RepresentativeUser)
	}
}

func TestOrganizationRepositoryListTenantMembersPageSearch(t *testing.T) {
	repo := newOrganizationMembersPageTestRepository(t)
	seedOrganizationMembersPageTestData(t, repo)

	tests := []struct {
		name   string
		query  string
		wantID string
	}{
		{name: "tenant name", query: "beta", wantID: "member-b"},
		{name: "representative display name", query: "display target", wantID: "member-b"},
		{name: "literal wildcard", query: "100%", wantID: "member-c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			members, total, err := repo.ListTenantMembersPage(context.Background(), "org-a", tt.query, 0, 20)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if total != 1 || len(members) != 1 || members[0].ID != tt.wantID {
				t.Fatalf("query %q returned total=%d members=%#v", tt.query, total, members)
			}
		})
	}
}

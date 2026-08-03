package mobileknowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", databaseName)), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE knowledge_bases (id text primary key, tenant_id integer, creator_id text, deleted_at datetime)`,
		`CREATE TABLE organizations (id text primary key, name text, description text, avatar text, owner_tenant_id integer, deleted_at datetime)`,
		`CREATE TABLE organization_tenant_members (organization_id text, tenant_id integer, role text)`,
		`CREATE TABLE kb_shares (id text primary key, knowledge_base_id text, organization_id text, shared_by_user_id text, source_tenant_id integer, permission text, deleted_at datetime)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func roleContext(role types.TenantRole) context.Context {
	return context.WithValue(context.Background(), types.TenantRoleContextKey, role)
}

func seedOrganization(t *testing.T, db *gorm.DB, id, name string, ownerTenant, memberTenant uint64, role types.OrgMemberRole) {
	t.Helper()
	if err := db.Exec(`INSERT INTO organizations(id,name,description,avatar,owner_tenant_id) VALUES(?,?,?,?,?)`, id, name, "", "", ownerTenant).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO organization_tenant_members(organization_id,tenant_id,role) VALUES(?,?,?)`, id, memberTenant, role).Error; err != nil {
		t.Fatal(err)
	}
}

func TestListShareTargetsFiltersBeforePaging(t *testing.T) {
	db := testDB(t)
	if err := db.Exec(`INSERT INTO knowledge_bases(id,tenant_id,creator_id) VALUES('kb-source',1,'user-1')`).Error; err != nil {
		t.Fatal(err)
	}
	seedOrganization(t, db, "org-owner", "甲空间", 1, 1, types.OrgRoleAdmin)
	seedOrganization(t, db, "org-admin", "乙空间", 9, 1, types.OrgRoleAdmin)
	seedOrganization(t, db, "org-editor", "丙空间", 9, 1, types.OrgRoleEditor)
	seedOrganization(t, db, "org-viewer", "丁空间", 9, 1, types.OrgRoleViewer)
	if err := db.Exec(`INSERT INTO kb_shares(id,knowledge_base_id,organization_id,shared_by_user_id,source_tenant_id,permission) VALUES('share-1','kb-source','org-admin','user-1',1,'viewer')`).Error; err != nil {
		t.Fatal(err)
	}

	page, err := NewService(db).ListShareTargets(
		roleContext(types.TenantRoleContributor), "kb-source", "user-1", 1, "", 1, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || !page.HasMore || len(page.Items) != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
	if page.Items[0].ID != "org-owner" || page.Items[1].ID != "org-admin" {
		t.Fatalf("permission-aware order mismatch: %+v", page.Items)
	}
	if page.Items[1].ShareID != "share-1" || !page.Items[1].CanRemove {
		t.Fatalf("existing share metadata missing: %+v", page.Items[1])
	}
	secondPage, err := NewService(db).ListShareTargets(
		roleContext(types.TenantRoleContributor), "kb-source", "user-1", 1, "", 2, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if secondPage.HasMore || len(secondPage.Items) != 1 || secondPage.Items[0].ID != "org-editor" {
		t.Fatalf("unauthorized rows leaked into a later page: %+v", secondPage)
	}

	searched, err := NewService(db).ListShareTargets(
		roleContext(types.TenantRoleContributor), "kb-source", "user-1", 1, "丙", 1, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if searched.Total != 1 || len(searched.Items) != 1 || searched.Items[0].ID != "org-editor" {
		t.Fatalf("search result mismatch: %+v", searched)
	}
}

func TestListShareTargetsIncomingOnlyReturnsGovernableShares(t *testing.T) {
	db := testDB(t)
	if err := db.Exec(`INSERT INTO knowledge_bases(id,tenant_id,creator_id) VALUES('kb-incoming',2,'source-user')`).Error; err != nil {
		t.Fatal(err)
	}
	seedOrganization(t, db, "org-admin", "管理空间", 9, 1, types.OrgRoleAdmin)
	seedOrganization(t, db, "org-editor", "编辑空间", 9, 1, types.OrgRoleEditor)
	for _, row := range []string{
		`INSERT INTO kb_shares(id,knowledge_base_id,organization_id,shared_by_user_id,source_tenant_id,permission) VALUES('share-admin','kb-incoming','org-admin','source-user',2,'viewer')`,
		`INSERT INTO kb_shares(id,knowledge_base_id,organization_id,shared_by_user_id,source_tenant_id,permission) VALUES('share-editor','kb-incoming','org-editor','source-user',2,'viewer')`,
	} {
		if err := db.Exec(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	page, err := NewService(db).ListShareTargets(
		roleContext(types.TenantRoleContributor), "kb-incoming", "target-user", 1, "", 1, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if page.Mode != "manage" || page.Total != 1 || page.Items[0].ID != "org-admin" || !page.Items[0].CanRemove {
		t.Fatalf("incoming governance filter mismatch: %+v", page)
	}
}

func TestListShareTargetsRejectsNonOwnerContributor(t *testing.T) {
	db := testDB(t)
	if err := db.Exec(`INSERT INTO knowledge_bases(id,tenant_id,creator_id) VALUES('kb-other',1,'other-user')`).Error; err != nil {
		t.Fatal(err)
	}
	_, err := NewService(db).ListShareTargets(
		roleContext(types.TenantRoleContributor), "kb-other", "user-1", 1, "", 1, 20,
	)
	if !errors.Is(err, ErrShareTargetForbidden) {
		t.Fatalf("expected forbidden, got %v", err)
	}
}

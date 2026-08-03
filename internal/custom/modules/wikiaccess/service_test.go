package wikiaccess

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.Exec(`
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			display_name TEXT,
			tenant_id INTEGER,
			is_active BOOLEAN NOT NULL DEFAULT TRUE,
			is_system_admin BOOLEAN NOT NULL DEFAULT FALSE,
			created_at DATETIME,
			deleted_at DATETIME
		)
	`).Error)
	require.NoError(t, db.Exec(`
		CREATE TABLE tenants (
			id INTEGER PRIMARY KEY,
			name TEXT,
			deleted_at DATETIME
		)
	`).Error)
	return db
}

func TestPermissionDefaultsToDeniedAndCanBeGrantedAndRevoked(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	require.NoError(t, service.Migrate(context.Background()))
	require.NoError(t, db.Exec(`INSERT INTO tenants (id, name) VALUES (7, '研发空间')`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO users (id, username, display_name, tenant_id, is_active, is_system_admin)
		VALUES ('user-1', 'alice', 'Alice', 7, TRUE, FALSE)
	`).Error)

	enabled, err := service.IsEnabled(context.Background(), "user-1")
	require.NoError(t, err)
	require.False(t, enabled)
	require.Error(t, service.GuardWikiSelection(context.Background(), "user-1"))

	row, err := service.SetUserPermission(context.Background(), "user-1", true, "admin-1")
	require.NoError(t, err)
	require.True(t, row.WikiEnabled)
	require.Equal(t, "研发空间", row.TenantName)
	require.NoError(t, service.GuardWikiSelection(context.Background(), "user-1"))

	row, err = service.SetUserPermission(context.Background(), "user-1", false, "admin-2")
	require.NoError(t, err)
	require.False(t, row.WikiEnabled)
	require.Error(t, service.GuardWikiSelection(context.Background(), "user-1"))
}

func TestConcurrentPermissionUpdatesRemainSingleRow(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	require.NoError(t, service.Migrate(context.Background()))
	require.NoError(t, db.Exec(`
		INSERT INTO users (id, username, tenant_id, is_active, is_system_admin)
		VALUES ('user-1', 'alice', 7, TRUE, FALSE)
	`).Error)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(enabled bool) {
			defer wg.Done()
			_, _ = service.SetUserPermission(context.Background(), "user-1", enabled, "admin-1")
		}(i%2 == 0)
	}
	wg.Wait()

	var count int64
	require.NoError(t, db.Model(&UserPermission{}).Where("user_id = ?", "user-1").Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestSearchUsersEscapesWildcardInput(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	require.NoError(t, service.Migrate(context.Background()))
	require.NoError(t, db.Exec(`
		INSERT INTO users (id, username, tenant_id, is_active, is_system_admin) VALUES
		('user-1', 'name_with_percent%', 7, TRUE, FALSE),
		('user-2', 'ordinary', 7, TRUE, FALSE)
	`).Error)

	result, err := service.SearchUsers(context.Background(), "%", 1, 20)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Total)
	require.Len(t, result.Users, 1)
	require.Equal(t, "user-1", result.Users[0].ID)
}

func TestSearchUsersPaginatesAndReturnsTotal(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	require.NoError(t, service.Migrate(context.Background()))
	for i := 1; i <= 25; i++ {
		require.NoError(t, db.Exec(`
			INSERT INTO users (id, username, tenant_id, is_active, is_system_admin)
			VALUES (?, ?, 7, TRUE, FALSE)
		`, fmt.Sprintf("user-%02d", i), fmt.Sprintf("user-%02d", i)).Error)
	}

	result, err := service.SearchUsers(context.Background(), "", 2, 20)
	require.NoError(t, err)
	require.EqualValues(t, 25, result.Total)
	require.Equal(t, 2, result.Page)
	require.Equal(t, 20, result.PageSize)
	require.Len(t, result.Users, 5)
	require.Equal(t, "user-21", result.Users[0].ID)
}

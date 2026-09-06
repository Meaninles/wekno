package testsupport

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Postgres creates an isolated schema in an explicitly selected test database.
// No test migrates or deletes tables in the database's normal search path.
func Postgres(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("WEKNORA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WEKNORA_TEST_POSTGRES_DSN is not configured")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	schema := "agent_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	t.Cleanup(func() {
		require.NoError(t, admin.Exec(`DROP SCHEMA "`+schema+`" CASCADE`).Error)
		conn, err := admin.DB()
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "search_path=" + schema
	} else {
		dsn += " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { conn, err := db.DB(); require.NoError(t, err); require.NoError(t, conn.Close()) })
	if len(models) > 0 {
		require.NoError(t, db.AutoMigrate(models...))
	}
	return db
}

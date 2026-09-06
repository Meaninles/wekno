package knowledgeaux

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func exercisePublicationMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	// This database already records a higher native version. Custom schema
	// installation must not depend on rerunning or rolling back native SQL.
	require.NoError(t, db.Exec(`CREATE TABLE schema_migrations (version BIGINT, dirty BOOLEAN)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO schema_migrations VALUES (102, false)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE knowledges (id VARCHAR(36) PRIMARY KEY,
		tenant_id BIGINT, knowledge_base_id VARCHAR(36), embedding_model_id VARCHAR(64), processing_generation VARCHAR(36))`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE chunks (id VARCHAR(36), tenant_id BIGINT,
		knowledge_id VARCHAR(36), processing_generation VARCHAR(36), is_enabled BOOLEAN, deleted_at TIMESTAMP, created_at TIMESTAMP)`).Error)
	if db.Dialector.Name() == "postgres" {
		require.NoError(t, db.Exec(`CREATE TABLE embeddings (chunk_id VARCHAR(36), is_enabled BOOLEAN)`).Error)
	}
	require.NoError(t, db.Exec(`INSERT INTO knowledges VALUES ('document', 7, 'kb', 'embedding', 'new-draft')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO chunks VALUES
		('old', 7, 'document', 'readable', true, NULL, '2026-01-01'),
		('new', 7, 'document', 'new-draft', false, NULL, '2026-02-01'),
		('other-tenant', 8, 'document', 'private', true, NULL, '2026-03-01')`).Error)
	require.NoError(t, Migrate(ctx, db))
	if db.Dialector.Name() == "postgres" {
		require.True(t, db.Migrator().HasIndex("embeddings", "idx_embeddings_chunk_id"))
	}
	var row types.Knowledge
	require.NoError(t, db.Unscoped().Table("knowledges").Where("id = ?", "document").Take(&row).Error)
	require.Equal(t, "readable", row.PublishedGeneration)
	require.Equal(t, "embedding", row.PublishedEmbeddingModelID)
	require.Equal(t, "published", row.PublicationState)
	require.NoError(t, db.Exec(`UPDATE knowledges SET published_generation = 'later-published'`).Error)
	require.NoError(t, Migrate(ctx, db))
	require.NoError(t, db.Unscoped().Table("knowledges").Where("id = ?", "document").Take(&row).Error)
	require.Equal(t, "later-published", row.PublishedGeneration, "restarting migration cannot revert an already published generation")
	var version int
	require.NoError(t, db.Table("schema_migrations").Select("version").Scan(&version).Error)
	require.Equal(t, 102, version)
}

func TestRepairPublicationMigrationOnExistingDatabase(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sql, err := db.DB()
	require.NoError(t, err)
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sql.Close() })
	exercisePublicationMigration(t, db)
}

func TestRepairPostgresPublicationMigrationOnExistingDatabase(t *testing.T) {
	exercisePublicationMigration(t, testsupport.Postgres(t))
}

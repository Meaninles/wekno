package knowledgepurge

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var postgresPurgeSequence atomic.Uint64

func openPurgePostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("WEKNORA_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("WEKNORA_TEST_POSTGRES_DSN is not configured")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	schema := fmt.Sprintf("knowledgepurge_%d_%d", time.Now().UnixNano(), postgresPurgeSequence.Add(1))
	require.NoError(t, admin.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	t.Cleanup(func() {
		require.NoError(t, admin.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	})
	if strings.Contains(dsn, "://") {
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		dsn += separator + "search_path=" + schema
	} else {
		dsn += " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	for _, statement := range []string{
		`CREATE TABLE knowledges (
			id VARCHAR(64) PRIMARY KEY, tenant_id BIGINT NOT NULL
		)`,
		`CREATE TABLE custom_content_cache_entries (
			tenant_id BIGINT NOT NULL, cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL, version_hash TEXT NOT NULL,
			ref_count BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (tenant_id, cache_kind, content_hash, version_hash)
		)`,
		`CREATE TABLE custom_content_cache_refs (
			tenant_id BIGINT NOT NULL, knowledge_id VARCHAR(64) NOT NULL,
			processing_generation TEXT NOT NULL, cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL, version_hash TEXT NOT NULL
		)`,
		`CREATE TABLE knowledge_tag_relations (knowledge_id VARCHAR(64) NOT NULL)`,
		`CREATE TABLE knowledge_fanout_completions (
			tenant_id BIGINT NOT NULL, knowledge_id VARCHAR(64) NOT NULL
		)`,
		`CREATE TABLE custom_enrichment_outcomes (
			tenant_id BIGINT NOT NULL, knowledge_id VARCHAR(64) NOT NULL
		)`,
		`CREATE TABLE custom_generated_question_claims (
			tenant_id BIGINT NOT NULL, knowledge_id VARCHAR(64) NOT NULL
		)`,
		`CREATE TABLE knowledge_processing_spans (knowledge_id VARCHAR(64) NOT NULL)`,
		`CREATE TABLE custom_document_split_parts (
			tenant_id BIGINT NOT NULL, knowledge_id VARCHAR(64) NOT NULL
		)`,
		`CREATE TABLE custom_document_split_plans (
			tenant_id BIGINT NOT NULL, knowledge_id VARCHAR(64) NOT NULL
		)`,
		`CREATE TABLE wiki_log_entries (
			tenant_id BIGINT NOT NULL, knowledge_id VARCHAR(64) NOT NULL
		)`,
		`CREATE TABLE embeddings (
			id BIGSERIAL PRIMARY KEY, source_id TEXT, source_type TEXT,
			chunk_id TEXT, knowledge_id VARCHAR(64) NOT NULL,
			knowledge_base_id VARCHAR(64), content TEXT
		)`,
		`CREATE TABLE chunks (
			id VARCHAR(64) PRIMARY KEY, tenant_id BIGINT NOT NULL,
			knowledge_id VARCHAR(64) NOT NULL, content TEXT, deleted_at TIMESTAMPTZ
		)`,
		`CREATE TABLE task_pending_ops (
			id BIGSERIAL PRIMARY KEY, tenant_id BIGINT NOT NULL,
			task_type VARCHAR(64) NOT NULL, scope VARCHAR(32) NOT NULL,
			scope_id VARCHAR(64) NOT NULL, op VARCHAR(32) NOT NULL,
			dedup_key VARCHAR(128) NOT NULL, payload JSONB NOT NULL DEFAULT '{}'
		)`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	return db
}

func TestPostgresDeleteSoftRowArtifactsMatchesProductionEmbeddingSchema(t *testing.T) {
	db := openPurgePostgresDB(t)
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (id, tenant_id)
		VALUES ('target', 7), ('foreign', 8);
		INSERT INTO embeddings (knowledge_id, knowledge_base_id, content)
		VALUES ('target', 'kb-7', 'private'), ('foreign', 'kb-8', 'foreign');
		INSERT INTO chunks (id, tenant_id, knowledge_id, content)
		VALUES ('chunk-target', 7, 'target', 'private'),
		       ('chunk-foreign', 8, 'foreign', 'foreign');
		INSERT INTO task_pending_ops
		  (tenant_id, task_type, scope, scope_id, op, dedup_key)
		VALUES
		  (7, 'knowledge:aux_object', 'knowledge_base', 'kb-7',
		   'delete_complete', 'target:proof'),
		  (7, 'knowledge:aux_object', 'knowledge_base', 'kb-7',
		   'owned', 'target:unfinished'),
		  (8, 'knowledge:aux_object', 'knowledge_base', 'kb-8',
		   'delete_complete', 'foreign:proof')`).Error)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return DeleteSoftRowArtifacts(tx, 7, []string{"target"})
	}))

	for table, query := range map[string]string{
		"target embedding": "SELECT COUNT(*) FROM embeddings WHERE knowledge_id = 'target'",
		"target chunk":     "SELECT COUNT(*) FROM chunks WHERE knowledge_id = 'target'",
		"completed proof":  "SELECT COUNT(*) FROM task_pending_ops WHERE dedup_key = 'target:proof'",
	} {
		var count int64
		require.NoError(t, db.Raw(query).Scan(&count).Error, table)
		require.Zero(t, count, table)
	}
	for table, query := range map[string]string{
		"foreign embedding": "SELECT COUNT(*) FROM embeddings WHERE knowledge_id = 'foreign'",
		"foreign chunk":     "SELECT COUNT(*) FROM chunks WHERE knowledge_id = 'foreign'",
		"foreign proof":     "SELECT COUNT(*) FROM task_pending_ops WHERE dedup_key = 'foreign:proof'",
		"unfinished proof":  "SELECT COUNT(*) FROM task_pending_ops WHERE dedup_key = 'target:unfinished'",
	} {
		var count int64
		require.NoError(t, db.Raw(query).Scan(&count).Error, table)
		require.EqualValues(t, 1, count, table)
	}
}

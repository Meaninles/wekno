package knowledgepurge

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newPurgeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(fmt.Sprintf("file:knowledge-purge-%s?mode=memory&cache=shared", t.Name())),
		&gorm.Config{},
	)
	require.NoError(t, err)
	for _, statement := range []string{
		`CREATE TABLE custom_content_cache_entries (
			tenant_id INTEGER NOT NULL, cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL, version_hash TEXT NOT NULL,
			ref_count INTEGER NOT NULL, updated_at DATETIME NOT NULL,
			PRIMARY KEY (tenant_id, cache_kind, content_hash, version_hash)
		)`,
		`CREATE TABLE custom_content_cache_refs (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL, cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL, version_hash TEXT NOT NULL
		)`,
		`CREATE TABLE knowledge_tag_relations (knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE knowledge_fanout_completions (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE custom_enrichment_outcomes (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE custom_generated_question_claims (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE knowledge_processing_spans (knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE custom_document_split_parts (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE custom_document_split_plans (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE wiki_log_entries (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE knowledges (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL
		)`,
		`CREATE TABLE task_pending_ops (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tenant_id INTEGER NOT NULL, task_type TEXT NOT NULL,
			scope TEXT NOT NULL, scope_id TEXT NOT NULL,
			op TEXT NOT NULL, dedup_key TEXT NOT NULL,
			payload JSON NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE embeddings (
			knowledge_id TEXT NOT NULL,
			content TEXT NOT NULL, deleted_at DATETIME
		)`,
		`CREATE TABLE chunks (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			content TEXT NOT NULL, deleted_at DATETIME
		)`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	return db
}

func TestDeleteSoftRowArtifactsPurgesTargetAndReleasesSharedCache(t *testing.T) {
	db := newPurgeDB(t)
	now := time.Now().UTC()
	require.NoError(t, db.Exec(
		"INSERT INTO knowledges (id, tenant_id) VALUES ('target', 7), ('other', 8)",
	).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO custom_content_cache_entries
		  (tenant_id, cache_kind, content_hash, version_hash, ref_count, updated_at)
		VALUES
		  (7, 'wiki_map', 'shared', 'v1', 2, ?),
		  (7, 'summary', 'unique', 'v1', 1, ?)`, now, now).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO custom_content_cache_refs
		  (tenant_id, knowledge_id, processing_generation, cache_kind, content_hash, version_hash)
		VALUES
		  (7, 'target', 'g1', 'wiki_map', 'shared', 'v1'),
		  (7, 'other',  'g2', 'wiki_map', 'shared', 'v1'),
		  (7, 'target', 'g1', 'summary',  'unique', 'v1')`).Error)

	for _, table := range []string{
		"knowledge_fanout_completions",
		"custom_enrichment_outcomes",
		"custom_generated_question_claims",
		"custom_document_split_parts",
		"custom_document_split_plans",
		"wiki_log_entries",
	} {
		require.NoError(t, db.Exec(
			"INSERT INTO "+table+" (tenant_id, knowledge_id) VALUES (7, 'target'), (7, 'other')",
		).Error)
	}
	require.NoError(t, db.Exec(
		"INSERT INTO knowledge_tag_relations (knowledge_id) VALUES ('target'), ('other')",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO knowledge_processing_spans (knowledge_id) VALUES ('target'), ('other')",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO embeddings (knowledge_id, content, deleted_at) "+
			"VALUES ('target', 'private parsed content', CURRENT_TIMESTAMP), "+
			"('other', 'other content', NULL)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO chunks (tenant_id, knowledge_id, content, deleted_at) "+
			"VALUES (7, 'target', 'private parsed content', CURRENT_TIMESTAMP), "+
			"(7, 'other', 'other content', NULL)",
	).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO task_pending_ops
		  (tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		VALUES
		  (7, 'knowledge:aux_object', 'knowledge_base', 'kb-target',
		   'delete_complete', 'target:source-hash', '{}'),
		  (7, 'knowledge:aux_object', 'knowledge_base', 'kb-target',
		   'owned', 'target:unfinished-hash', '{}'),
		  (7, 'knowledge:aux_object', 'knowledge_base', 'kb-other',
		   'delete_complete', 'other:source-hash', '{}')`).Error)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return DeleteSoftRowArtifacts(tx, 7, []string{"target"})
	}))

	for _, table := range []string{
		"knowledge_tag_relations",
		"knowledge_fanout_completions",
		"custom_enrichment_outcomes",
		"custom_generated_question_claims",
		"knowledge_processing_spans",
		"custom_document_split_parts",
		"custom_document_split_plans",
		"wiki_log_entries",
		"custom_content_cache_refs",
		"embeddings",
		"chunks",
	} {
		var targetCount, otherCount int64
		require.NoError(t, db.Table(table).Where("knowledge_id = ?", "target").Count(&targetCount).Error)
		require.NoError(t, db.Table(table).Where("knowledge_id = ?", "other").Count(&otherCount).Error)
		require.Zero(t, targetCount, table)
		require.EqualValues(t, 1, otherCount, table)
	}

	var sharedRefs, uniqueRefs int64
	require.NoError(t, db.Table("custom_content_cache_entries").
		Where("content_hash = ?", "shared").Select("ref_count").Scan(&sharedRefs).Error)
	require.NoError(t, db.Table("custom_content_cache_entries").
		Where("content_hash = ?", "unique").Select("ref_count").Scan(&uniqueRefs).Error)
	require.EqualValues(t, 1, sharedRefs)
	require.Zero(t, uniqueRefs)

	var completedTarget, ownedTarget, completedOther int64
	require.NoError(t, db.Table("task_pending_ops").
		Where("dedup_key = ?", "target:source-hash").Count(&completedTarget).Error)
	require.NoError(t, db.Table("task_pending_ops").
		Where("dedup_key = ?", "target:unfinished-hash").Count(&ownedTarget).Error)
	require.NoError(t, db.Table("task_pending_ops").
		Where("dedup_key = ?", "other:source-hash").Count(&completedOther).Error)
	require.Zero(t, completedTarget, "finalize consumes only the exact completed proof")
	require.EqualValues(t, 1, ownedTarget, "unfinished ownership proof must remain recoverable")
	require.EqualValues(t, 1, completedOther, "another document's proof must remain isolated")
}

func TestDeleteSoftRowArtifactsRollsBackWhenCacheEntryIsMissing(t *testing.T) {
	db := newPurgeDB(t)
	require.NoError(t, db.Exec(
		"INSERT INTO knowledges (id, tenant_id) VALUES ('target', 7)",
	).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO custom_content_cache_refs
		  (tenant_id, knowledge_id, processing_generation, cache_kind, content_hash, version_hash)
		VALUES (7, 'target', 'g1', 'summary', 'missing', 'v1')`).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO knowledge_processing_spans (knowledge_id) VALUES ('target')",
	).Error)

	err := db.Transaction(func(tx *gorm.DB) error {
		return DeleteSoftRowArtifacts(tx, 7, []string{"target"})
	})
	require.ErrorContains(t, err, "immutable entry")

	var refCount, spanCount int64
	require.NoError(t, db.Table("custom_content_cache_refs").Count(&refCount).Error)
	require.NoError(t, db.Table("knowledge_processing_spans").Count(&spanCount).Error)
	require.EqualValues(t, 1, refCount, "failed purge must roll back reference deletion")
	require.EqualValues(t, 1, spanCount, "failed purge must not partially delete derived rows")
}

// Package knowledgepurge removes database artifacts that cannot rely on
// foreign-key cascades because Knowledge uses a soft-delete tombstone.
package knowledgepurge

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

type cacheReferenceGroup struct {
	CacheKind      string `gorm:"column:cache_kind"`
	ContentHash    string `gorm:"column:content_hash"`
	VersionHash    string `gorm:"column:version_hash"`
	ReferenceCount int64  `gorm:"column:reference_count"`
}

// DeleteSoftRowArtifacts removes all knowledge-scoped relational artifacts
// in the caller's transaction. Shared immutable cache payloads are retained,
// but their reference counts are decremented atomically; the bounded cache
// sweeper can then reclaim entries whose count reaches zero.
func DeleteSoftRowArtifacts(tx *gorm.DB, tenantID uint64, knowledgeIDs []string) error {
	if tx == nil || tenantID == 0 {
		return errors.New("knowledge purge requires a database transaction and tenant")
	}
	ids, err := normalizedIDs(knowledgeIDs)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	var cacheGroups []cacheReferenceGroup
	if err := tx.Table("custom_content_cache_refs").
		Select("cache_kind, content_hash, version_hash, COUNT(*) AS reference_count").
		Where("tenant_id = ? AND knowledge_id IN ?", tenantID, ids).
		Group("cache_kind, content_hash, version_hash").
		Scan(&cacheGroups).Error; err != nil {
		return fmt.Errorf("list knowledge content-cache references: %w", err)
	}
	if err := tx.Exec(
		"DELETE FROM custom_content_cache_refs WHERE tenant_id = ? AND knowledge_id IN ?",
		tenantID, ids,
	).Error; err != nil {
		return fmt.Errorf("delete knowledge content-cache references: %w", err)
	}
	now := time.Now().UTC()
	for _, group := range cacheGroups {
		if group.ReferenceCount <= 0 {
			return errors.New("knowledge purge observed an invalid content-cache reference count")
		}
		result := tx.Exec(`
			UPDATE custom_content_cache_entries
			   SET ref_count = CASE
			         WHEN ref_count >= ? THEN ref_count - ?
			         ELSE 0
			       END,
			       updated_at = ?
			 WHERE tenant_id = ?
			   AND cache_kind = ?
			   AND content_hash = ?
			   AND version_hash = ?`,
			group.ReferenceCount, group.ReferenceCount, now,
			tenantID, group.CacheKind, group.ContentHash, group.VersionHash,
		)
		if result.Error != nil {
			return fmt.Errorf("decrement knowledge content-cache reference count: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf(
				"decrement knowledge content-cache reference count: immutable entry %s/%s/%s is missing",
				group.CacheKind, group.ContentHash, group.VersionHash,
			)
		}
	}

	statements := []struct {
		name  string
		query string
		args  []interface{}
	}{
		{"knowledge tag relations", "DELETE FROM knowledge_tag_relations WHERE knowledge_id IN ?", []interface{}{ids}},
		{"fanout completions", "DELETE FROM knowledge_fanout_completions WHERE tenant_id = ? AND knowledge_id IN ?", []interface{}{tenantID, ids}},
		{"enrichment outcomes", "DELETE FROM custom_enrichment_outcomes WHERE tenant_id = ? AND knowledge_id IN ?", []interface{}{tenantID, ids}},
		{"generated question claims", "DELETE FROM custom_generated_question_claims WHERE tenant_id = ? AND knowledge_id IN ?", []interface{}{tenantID, ids}},
		{"processing spans", "DELETE FROM knowledge_processing_spans WHERE knowledge_id IN ?", []interface{}{ids}},
		{"document split parts", "DELETE FROM custom_document_split_parts WHERE tenant_id = ? AND knowledge_id IN ?", []interface{}{tenantID, ids}},
		{"document split plans", "DELETE FROM custom_document_split_plans WHERE tenant_id = ? AND knowledge_id IN ?", []interface{}{tenantID, ids}},
		{"Wiki operation logs", "DELETE FROM wiki_log_entries WHERE tenant_id = ? AND knowledge_id IN ?", []interface{}{tenantID, ids}},
		// Chunk cleanup is soft during the external-resource phase so a retry
		// can still discover image provenance. Finalize runs only after vectors,
		// objects, graph data, and Wiki quarantine have all succeeded, and it
		// owns this transaction. Physically purge both relational search rows
		// here so a user-visible deletion does not retain parsed text merely as
		// a GORM tombstone. Any later transaction failure restores both tables.
		// The production embeddings schema intentionally has no tenant_id
		// column. Scope it through the still-present knowledge tombstone instead
		// of trusting a globally unique UUID alone.
		{"embeddings", `DELETE FROM embeddings
			WHERE knowledge_id IN (
				SELECT id FROM knowledges WHERE tenant_id = ? AND id IN ?
			)`, []interface{}{tenantID, ids}},
		{"chunks", "DELETE FROM chunks WHERE tenant_id = ? AND knowledge_id IN ?", []interface{}{tenantID, ids}},
	}
	for _, statement := range statements {
		if err := tx.Exec(statement.query, statement.args...).Error; err != nil {
			return fmt.Errorf("delete %s: %w", statement.name, err)
		}
	}
	// Provider deletion is an external side effect. CleanupForDelete changes
	// each exact ownership row into a durable delete-complete proof so a crash
	// or a later subsystem failure can retry without treating the now-absent
	// object as an unregistered path. Consume only those completed proofs in
	// the same transaction that finalizes the document tombstone; an "owned"
	// row is deliberately retained so recovery can never hide an object that
	// was not proven deleted.
	for _, knowledgeID := range ids {
		prefix := knowledgeID + ":"
		if err := tx.Exec(`
			DELETE FROM task_pending_ops
			 WHERE tenant_id = ?
			   AND task_type = 'knowledge:aux_object'
			   AND scope = 'knowledge_base'
			   AND op = 'delete_complete'
			   AND substr(dedup_key, 1, length(?)) = ?`,
			tenantID, prefix, prefix,
		).Error; err != nil {
			return fmt.Errorf("delete knowledge auxiliary completion proofs: %w", err)
		}
	}
	return nil
}

func normalizedIDs(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	ids := make([]string, 0, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" {
			return nil, errors.New("knowledge purge IDs must be non-empty")
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("knowledge purge ID %q is duplicated", id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

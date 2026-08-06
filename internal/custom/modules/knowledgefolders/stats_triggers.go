package knowledgefolders

import (
	"context"
	"fmt"
)

func folderStatsTerminalFailureSQL(row string) string {
	parseStatus := "LOWER(COALESCE(" + row + ".parse_status, ''))"
	summaryStatus := "LOWER(COALESCE(" + row + ".summary_status, ''))"
	enrichmentStatus := "LOWER(COALESCE(" + row + ".enrichment_status, ''))"
	wikiStatus := "LOWER(COALESCE(" + row + ".wiki_status, ''))"
	active := "('pending', 'processing')"
	successOrDegraded := "('', 'none', 'completed', 'done', 'skipped', 'degraded')"

	return "(" + parseStatus + " = 'failed' OR (" +
		parseStatus + " = 'completed' AND " +
		summaryStatus + " NOT IN " + active + " AND " +
		enrichmentStatus + " NOT IN " + active + " AND " +
		wikiStatus + " NOT IN " + active + " AND (" +
		summaryStatus + " NOT IN " + successOrDegraded + " OR " +
		enrichmentStatus + " NOT IN " + successOrDegraded + " OR " +
		wikiStatus + " NOT IN " + successOrDegraded + ")))"
}

func folderStatsAbnormalSQL(row string) string {
	parseStatus := "LOWER(COALESCE(" + row + ".parse_status, ''))"
	enrichmentStatus := "LOWER(COALESCE(" + row + ".enrichment_status, ''))"
	wikiStatus := "LOWER(COALESCE(" + row + ".wiki_status, ''))"
	legacyAbnormal := "(" + parseStatus + " IN ('failed', 'cancelled') OR " +
		enrichmentStatus + " IN ('failed', 'degraded') OR " +
		wikiStatus + " IN ('failed', 'degraded'))"
	return "(" + legacyAbnormal + " AND NOT " + folderStatsTerminalFailureSQL(row) + ")"
}

func (s *Service) ensureStatsTriggers(ctx context.Context) error {
	switch s.db.Dialector.Name() {
	case "postgres":
		return s.ensurePostgresStatsTriggers(ctx)
	case "sqlite":
		return s.ensureSQLiteStatsTriggers(ctx)
	default:
		return fmt.Errorf("knowledge folder statistics do not support database dialect %q", s.db.Dialector.Name())
	}
}

func (s *Service) ensurePostgresStatsTriggers(ctx context.Context) error {
	sql := fmt.Sprintf(`
CREATE OR REPLACE FUNCTION custom_knowledge_folder_validate_placement()
RETURNS trigger AS $$
BEGIN
	IF COALESCE(NEW.folder_id, '') = '' THEN
		RETURN NEW;
	END IF;
	PERFORM 1
	FROM custom_knowledge_folders AS folder
	WHERE folder.id = NEW.folder_id
	  AND folder.tenant_id = NEW.tenant_id
	  AND folder.knowledge_base_id = NEW.knowledge_base_id
	  AND folder.delete_status <> 'deleting'
	FOR KEY SHARE;
	IF NOT FOUND THEN
		RAISE EXCEPTION 'knowledge folder is missing or deleting'
			USING ERRCODE = '23503';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_validate_placement ON knowledges;
CREATE TRIGGER trigger_custom_knowledge_folder_validate_placement
BEFORE INSERT OR UPDATE OF folder_id ON knowledges
FOR EACH ROW
EXECUTE FUNCTION custom_knowledge_folder_validate_placement();

CREATE OR REPLACE FUNCTION custom_knowledge_folder_apply_delta(
	p_folder_id varchar,
	p_documents bigint,
	p_parse_pending bigint,
	p_parse_running bigint,
	p_enrichment_pending bigint,
	p_wiki_pending bigint,
	p_abnormal bigint,
	p_failed bigint
) RETURNS void AS $$
BEGIN
	IF COALESCE(p_folder_id, '') = '' THEN
		RETURN;
	END IF;
	UPDATE custom_knowledge_folder_stats AS stats
	SET
		subtree_document_count = GREATEST(0, stats.subtree_document_count + p_documents),
		parse_pending_count = GREATEST(0, stats.parse_pending_count + p_parse_pending),
		parse_running_count = GREATEST(0, stats.parse_running_count + p_parse_running),
		enrichment_pending_task_count = GREATEST(0, stats.enrichment_pending_task_count + p_enrichment_pending),
		wiki_pending_task_count = GREATEST(0, stats.wiki_pending_task_count + p_wiki_pending),
		abnormal_document_count = GREATEST(0, stats.abnormal_document_count + p_abnormal),
		failed_document_count = GREATEST(0, stats.failed_document_count + p_failed),
		updated_at = CURRENT_TIMESTAMP
	FROM custom_knowledge_folder_closure AS closure
	WHERE closure.descendant_id = p_folder_id
	  AND closure.ancestor_id = stats.folder_id;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION custom_knowledge_folder_project_knowledge()
RETURNS trigger AS $$
DECLARE
	old_active bigint := 0;
	new_active bigint := 0;
	old_pending bigint := 0;
	new_pending bigint := 0;
	old_running bigint := 0;
	new_running bigint := 0;
	old_enrichment bigint := 0;
	new_enrichment bigint := 0;
	old_wiki bigint := 0;
	new_wiki bigint := 0;
	old_abnormal bigint := 0;
	new_abnormal bigint := 0;
	old_failed bigint := 0;
	new_failed bigint := 0;
BEGIN
	IF TG_OP <> 'INSERT' AND OLD.deleted_at IS NULL AND COALESCE(OLD.folder_id, '') <> '' AND OLD.parse_status <> 'deleting' THEN
		old_active := 1;
		old_pending := CASE WHEN OLD.parse_status = 'pending' THEN 1 ELSE 0 END;
		old_running := CASE WHEN OLD.parse_status IN ('processing', 'cancelling') THEN 1 ELSE 0 END;
		old_enrichment := GREATEST(COALESCE(OLD.pending_subtasks_count, 0), 0);
		old_wiki := CASE WHEN COALESCE(OLD.wiki_status, '') = 'pending' THEN 1 ELSE 0 END;
		old_abnormal := CASE WHEN %s THEN 1 ELSE 0 END;
		old_failed := CASE WHEN %s THEN 1 ELSE 0 END;
		PERFORM custom_knowledge_folder_apply_delta(
			OLD.folder_id,
			-old_active, -old_pending, -old_running,
			-old_enrichment, -old_wiki, -old_abnormal, -old_failed
		);
	END IF;

	IF TG_OP <> 'DELETE' AND NEW.deleted_at IS NULL AND COALESCE(NEW.folder_id, '') <> '' AND NEW.parse_status <> 'deleting' THEN
		new_active := 1;
		new_pending := CASE WHEN NEW.parse_status = 'pending' THEN 1 ELSE 0 END;
		new_running := CASE WHEN NEW.parse_status IN ('processing', 'cancelling') THEN 1 ELSE 0 END;
		new_enrichment := GREATEST(COALESCE(NEW.pending_subtasks_count, 0), 0);
		new_wiki := CASE WHEN COALESCE(NEW.wiki_status, '') = 'pending' THEN 1 ELSE 0 END;
		new_abnormal := CASE WHEN %s THEN 1 ELSE 0 END;
		new_failed := CASE WHEN %s THEN 1 ELSE 0 END;
		PERFORM custom_knowledge_folder_apply_delta(
			NEW.folder_id,
			new_active, new_pending, new_running,
			new_enrichment, new_wiki, new_abnormal, new_failed
		);
	END IF;
	IF TG_OP = 'DELETE' THEN
		RETURN OLD;
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_project ON knowledges;
CREATE TRIGGER trigger_custom_knowledge_folder_project
AFTER INSERT OR DELETE OR UPDATE OF
	folder_id, deleted_at, parse_status, summary_status, pending_subtasks_count, enrichment_status, wiki_status
ON knowledges
FOR EACH ROW
EXECUTE FUNCTION custom_knowledge_folder_project_knowledge();

DROP FUNCTION IF EXISTS custom_knowledge_folder_apply_delta(
	varchar, bigint, bigint, bigint, bigint, bigint, bigint
);

CREATE OR REPLACE FUNCTION custom_knowledge_folder_cleanup_knowledge_base()
RETURNS trigger AS $$
DECLARE
	target_tenant_id bigint;
	target_kb_id varchar;
BEGIN
	IF TG_OP = 'UPDATE' AND NOT (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL) THEN
		RETURN NEW;
	END IF;
	target_tenant_id := OLD.tenant_id;
	target_kb_id := OLD.id;
	DELETE FROM custom_knowledge_folder_delete_items
	WHERE operation_id IN (
		SELECT id FROM custom_knowledge_folder_delete_operations
		WHERE tenant_id = target_tenant_id AND knowledge_base_id = target_kb_id
	);
	DELETE FROM custom_knowledge_folder_delete_operations
	WHERE tenant_id = target_tenant_id AND knowledge_base_id = target_kb_id;
	DELETE FROM custom_knowledge_folder_closure
	WHERE tenant_id = target_tenant_id AND knowledge_base_id = target_kb_id;
	DELETE FROM custom_knowledge_folder_stats
	WHERE tenant_id = target_tenant_id AND knowledge_base_id = target_kb_id;
	DELETE FROM custom_knowledge_folders
	WHERE tenant_id = target_tenant_id AND knowledge_base_id = target_kb_id;
	IF TG_OP = 'DELETE' THEN
		RETURN OLD;
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_cleanup_kb ON knowledge_bases;
CREATE TRIGGER trigger_custom_knowledge_folder_cleanup_kb
AFTER DELETE OR UPDATE OF deleted_at ON knowledge_bases
FOR EACH ROW
EXECUTE FUNCTION custom_knowledge_folder_cleanup_knowledge_base();
`,
		folderStatsAbnormalSQL("OLD"), folderStatsTerminalFailureSQL("OLD"),
		folderStatsAbnormalSQL("NEW"), folderStatsTerminalFailureSQL("NEW"),
	)
	if err := s.db.WithContext(ctx).Exec(sql).Error; err != nil {
		return fmt.Errorf("install PostgreSQL knowledge folder statistic triggers: %w", err)
	}
	return nil
}

func (s *Service) ensureSQLiteStatsTriggers(ctx context.Context) error {
	statUpdate := func(prefix string, sign int) string {
		folder := prefix + ".folder_id"
		active := prefix + ".deleted_at IS NULL AND COALESCE(" + folder + ", '') <> '' AND " + prefix + ".parse_status <> 'deleting'"
		multiplier := ""
		if sign < 0 {
			multiplier = "-"
		}
		return fmt.Sprintf(`
UPDATE custom_knowledge_folder_stats
SET
	subtree_document_count = MAX(0, subtree_document_count + %s1),
	parse_pending_count = MAX(0, parse_pending_count + %s(CASE WHEN %s.parse_status = 'pending' THEN 1 ELSE 0 END)),
	parse_running_count = MAX(0, parse_running_count + %s(CASE WHEN %s.parse_status IN ('processing', 'cancelling') THEN 1 ELSE 0 END)),
	enrichment_pending_task_count = MAX(0, enrichment_pending_task_count + %sMAX(COALESCE(%s.pending_subtasks_count, 0), 0)),
	wiki_pending_task_count = MAX(0, wiki_pending_task_count + %s(CASE WHEN COALESCE(%s.wiki_status, '') = 'pending' THEN 1 ELSE 0 END)),
	abnormal_document_count = MAX(0, abnormal_document_count + %s(CASE WHEN %s THEN 1 ELSE 0 END)),
	failed_document_count = MAX(0, failed_document_count + %s(CASE WHEN %s THEN 1 ELSE 0 END)),
	updated_at = CURRENT_TIMESTAMP
WHERE folder_id IN (
	SELECT ancestor_id FROM custom_knowledge_folder_closure WHERE descendant_id = %s
)
AND %s;`,
			multiplier,
			multiplier, prefix,
			multiplier, prefix,
			multiplier, prefix,
			multiplier, prefix,
			multiplier, folderStatsAbnormalSQL(prefix),
			multiplier, folderStatsTerminalFailureSQL(prefix),
			folder, active,
		)
	}
	sql := `
DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_insert;
DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_delete;
DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_update;
DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_validate_placement_insert;
DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_validate_placement_update;
DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_cleanup_kb_delete;
DROP TRIGGER IF EXISTS trigger_custom_knowledge_folder_cleanup_kb_update;

CREATE TRIGGER trigger_custom_knowledge_folder_validate_placement_insert
BEFORE INSERT ON knowledges
WHEN COALESCE(NEW.folder_id, '') <> '' AND NOT EXISTS (
	SELECT 1 FROM custom_knowledge_folders AS folder
	WHERE folder.id = NEW.folder_id
	  AND folder.tenant_id = NEW.tenant_id
	  AND folder.knowledge_base_id = NEW.knowledge_base_id
	  AND folder.delete_status <> 'deleting'
)
BEGIN
	SELECT RAISE(ABORT, 'knowledge folder is missing or deleting');
END;

CREATE TRIGGER trigger_custom_knowledge_folder_validate_placement_update
BEFORE UPDATE OF folder_id ON knowledges
WHEN COALESCE(NEW.folder_id, '') <> '' AND NOT EXISTS (
	SELECT 1 FROM custom_knowledge_folders AS folder
	WHERE folder.id = NEW.folder_id
	  AND folder.tenant_id = NEW.tenant_id
	  AND folder.knowledge_base_id = NEW.knowledge_base_id
	  AND folder.delete_status <> 'deleting'
)
BEGIN
	SELECT RAISE(ABORT, 'knowledge folder is missing or deleting');
END;

CREATE TRIGGER trigger_custom_knowledge_folder_insert
AFTER INSERT ON knowledges
BEGIN
` + statUpdate("NEW", 1) + `
END;

CREATE TRIGGER trigger_custom_knowledge_folder_delete
AFTER DELETE ON knowledges
BEGIN
` + statUpdate("OLD", -1) + `
END;

CREATE TRIGGER trigger_custom_knowledge_folder_update
AFTER UPDATE OF folder_id, deleted_at, parse_status, summary_status, pending_subtasks_count, enrichment_status, wiki_status ON knowledges
BEGIN
` + statUpdate("OLD", -1) + statUpdate("NEW", 1) + `
END;

CREATE TRIGGER trigger_custom_knowledge_folder_cleanup_kb_delete
AFTER DELETE ON knowledge_bases
BEGIN
	DELETE FROM custom_knowledge_folder_delete_items
	WHERE operation_id IN (
		SELECT id FROM custom_knowledge_folder_delete_operations
		WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id
	);
	DELETE FROM custom_knowledge_folder_delete_operations
	WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id;
	DELETE FROM custom_knowledge_folder_closure
	WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id;
	DELETE FROM custom_knowledge_folder_stats
	WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id;
	DELETE FROM custom_knowledge_folders
	WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id;
END;

CREATE TRIGGER trigger_custom_knowledge_folder_cleanup_kb_update
AFTER UPDATE OF deleted_at ON knowledge_bases
WHEN OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL
BEGIN
	DELETE FROM custom_knowledge_folder_delete_items
	WHERE operation_id IN (
		SELECT id FROM custom_knowledge_folder_delete_operations
		WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id
	);
	DELETE FROM custom_knowledge_folder_delete_operations
	WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id;
	DELETE FROM custom_knowledge_folder_closure
	WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id;
	DELETE FROM custom_knowledge_folder_stats
	WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id;
	DELETE FROM custom_knowledge_folders
	WHERE tenant_id = OLD.tenant_id AND knowledge_base_id = OLD.id;
END;
`
	if err := s.db.WithContext(ctx).Exec(sql).Error; err != nil {
		return fmt.Errorf("install SQLite knowledge folder statistic triggers: %w", err)
	}
	return nil
}

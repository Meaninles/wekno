-- One read-only row per live knowledge base. Run again only after every
-- state-changing app/worker Deployment has reached zero replicas.
--
-- A hybrid KB is detected from any of three independent signals:
--   1. indexing_strategy.wiki_enabled is true;
--   2. a live Wiki page exists;
--   3. at least one live document has ever entered a non-'none' Wiki state.
-- This deliberately does not rely on knowledge_bases.type: document + Wiki is
-- an additive configuration in current production.
WITH live_kb AS (
    SELECT
        kb.id,
        kb.tenant_id,
        kb.name,
        kb.type,
        kb.embedding_model_id,
        kb.vector_store_id,
        LOWER(COALESCE(kb.indexing_strategy->>'wiki_enabled', 'false')) = 'true'
            AS configured_wiki_enabled,
        kb.wiki_config IS NOT NULL AS has_wiki_config
    FROM knowledge_bases AS kb
    WHERE kb.deleted_at IS NULL
),
wiki_pages AS (
    SELECT knowledge_base_id, COUNT(*) AS live_wiki_pages
    FROM wiki_pages
    WHERE deleted_at IS NULL
    GROUP BY knowledge_base_id
),
doc_raw AS (
    SELECT
        k.*,
        LOWER(COALESCE(k.parse_status, '')) = 'completed'
            AND LOWER(COALESCE(k.summary_status, 'none')) IN ('none', 'completed')
            AND LOWER(COALESCE(k.enrichment_status, 'none')) IN ('none', 'completed')
            AND COALESCE(k.pending_subtasks_count, 0) = 0
            AS base_complete
    FROM knowledges AS k
    INNER JOIN live_kb AS kb ON kb.id = k.knowledge_base_id
    WHERE k.deleted_at IS NULL
),
doc_signals AS (
    SELECT
        knowledge_base_id,
        COUNT(*) AS document_count,
        COUNT(*) FILTER (
            WHERE LOWER(COALESCE(wiki_status, 'none')) <> 'none'
        ) AS documents_with_wiki_history,
        COUNT(*) FILTER (WHERE NULLIF(file_path, '') IS NOT NULL)
            AS documents_with_source_object,
        COUNT(*) FILTER (WHERE NULLIF(file_path, '') IS NULL)
            AS documents_without_source_object,
        COUNT(*) FILTER (WHERE LOWER(COALESCE(parse_status, '')) = 'completed')
            AS parse_completed,
        COUNT(*) FILTER (
            WHERE LOWER(COALESCE(summary_status, 'none')) NOT IN ('none', 'completed')
        ) AS summary_incomplete,
        COUNT(*) FILTER (
            WHERE LOWER(COALESCE(enrichment_status, 'none')) NOT IN ('none', 'completed')
        ) AS enrichment_incomplete,
        COUNT(*) FILTER (WHERE COALESCE(pending_subtasks_count, 0) <> 0)
            AS pending_subtask_documents,
        COUNT(*) FILTER (WHERE LOWER(COALESCE(enable_status, '')) = 'disabled')
            AS disabled_documents
    FROM doc_raw
    GROUP BY knowledge_base_id
),
kb_signals AS (
    SELECT
        kb.*,
        COALESCE(wp.live_wiki_pages, 0) AS live_wiki_pages,
        COALESCE(ds.document_count, 0) AS document_count,
        COALESCE(ds.documents_with_wiki_history, 0) AS documents_with_wiki_history,
        COALESCE(ds.documents_with_source_object, 0) AS documents_with_source_object,
        COALESCE(ds.documents_without_source_object, 0) AS documents_without_source_object,
        COALESCE(ds.parse_completed, 0) AS parse_completed,
        COALESCE(ds.summary_incomplete, 0) AS summary_incomplete,
        COALESCE(ds.enrichment_incomplete, 0) AS enrichment_incomplete,
        COALESCE(ds.pending_subtask_documents, 0) AS pending_subtask_documents,
        COALESCE(ds.disabled_documents, 0) AS disabled_documents,
        (
            kb.configured_wiki_enabled
            OR COALESCE(wp.live_wiki_pages, 0) > 0
            OR COALESCE(ds.documents_with_wiki_history, 0) > 0
        ) AS is_hybrid
    FROM live_kb AS kb
    LEFT JOIN wiki_pages AS wp ON wp.knowledge_base_id = kb.id
    LEFT JOIN doc_signals AS ds ON ds.knowledge_base_id = kb.id
),
evaluated AS (
    SELECT
        kb.*,
        COALESCE(COUNT(d.id) FILTER (
            WHERE d.base_complete
              AND (
                    (NOT kb.is_hybrid AND LOWER(COALESCE(d.wiki_status, 'none')) IN ('none', 'completed'))
                    OR (kb.is_hybrid AND LOWER(COALESCE(d.wiki_status, 'none')) = 'completed')
                  )
        ), 0) AS fully_complete_documents,
        COALESCE(COUNT(d.id) FILTER (
            WHERE NOT (
                d.base_complete
                AND (
                      (NOT kb.is_hybrid AND LOWER(COALESCE(d.wiki_status, 'none')) IN ('none', 'completed'))
                      OR (kb.is_hybrid AND LOWER(COALESCE(d.wiki_status, 'none')) = 'completed')
                    )
            )
        ), 0) AS noncomplete_documents,
        COALESCE(COUNT(d.id) FILTER (
            WHERE kb.is_hybrid
              AND LOWER(COALESCE(d.wiki_status, 'none')) <> 'completed'
        ), 0) AS wiki_incomplete_documents
    FROM kb_signals AS kb
    LEFT JOIN doc_raw AS d ON d.knowledge_base_id = kb.id
    GROUP BY
        kb.id, kb.tenant_id, kb.name, kb.type, kb.embedding_model_id,
        kb.vector_store_id, kb.configured_wiki_enabled, kb.has_wiki_config,
        kb.live_wiki_pages, kb.document_count, kb.documents_with_wiki_history,
        kb.documents_with_source_object, kb.documents_without_source_object,
        kb.parse_completed, kb.summary_incomplete,
        kb.enrichment_incomplete, kb.pending_subtask_documents,
        kb.disabled_documents, kb.is_hybrid
)
SELECT
    tenant_id,
    id AS knowledge_base_id,
    name AS knowledge_base_name,
    type AS knowledge_base_type,
    embedding_model_id,
    vector_store_id,
    configured_wiki_enabled,
    has_wiki_config,
    live_wiki_pages,
    documents_with_wiki_history,
    is_hybrid,
    document_count,
    fully_complete_documents,
    noncomplete_documents,
    documents_with_source_object,
    documents_without_source_object,
    parse_completed,
    summary_incomplete,
    enrichment_incomplete,
    wiki_incomplete_documents,
    pending_subtask_documents,
    disabled_documents,
    CASE
        WHEN type <> 'document' THEN 'MANUAL_REVIEW_NON_DOCUMENT'
        WHEN document_count = 0 THEN 'KEEP_EMPTY'
        WHEN noncomplete_documents = 0 THEN 'KEEP_ALL_COMPLETE'
        WHEN is_hybrid THEN 'CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST'
        ELSE 'REBUILD_DOCUMENT_KB_IN_PLACE'
    END AS release_action,
    CASE
        WHEN type = 'document' AND document_count > 0
             AND noncomplete_documents > 0 AND is_hybrid
            THEN name || '（非Wiki重建）'
        ELSE ''
    END AS replacement_name,
    CASE
        WHEN type <> 'document'
            THEN 'Production currently has no expected non-document KB; inspect manually.'
        WHEN document_count = 0
            THEN 'No document task exists; retain and verify configuration only.'
        WHEN noncomplete_documents = 0
            THEN 'Every live document has a successful terminal state; do not rebuild.'
        WHEN is_hybrid
            THEN 'Keep the old hybrid KB for rollback; create a pure-document target and re-ingest every source object.'
        ELSE 'Wiki is not enabled or historically materialized; manually batch-reparse every live document.'
    END AS release_reason
FROM evaluated
ORDER BY tenant_id, name, id;

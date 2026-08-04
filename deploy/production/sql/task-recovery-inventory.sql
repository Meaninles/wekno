-- Unified, read-only task recovery ledger. It intentionally omits raw payloads
-- and error strings: old payloads carry stale generations and must never be
-- blindly replayed after a migration.
WITH live_kb AS (
    SELECT
        kb.id,
        kb.type,
        (
            LOWER(COALESCE(kb.indexing_strategy->>'wiki_enabled', 'false')) = 'true'
            OR EXISTS (
                SELECT 1 FROM wiki_pages wp
                WHERE wp.knowledge_base_id = kb.id AND wp.deleted_at IS NULL
            )
            OR EXISTS (
                SELECT 1 FROM knowledges wk
                WHERE wk.knowledge_base_id = kb.id
                  AND wk.deleted_at IS NULL
                  AND LOWER(COALESCE(wk.wiki_status, 'none')) <> 'none'
            )
        ) AS is_hybrid
    FROM knowledge_bases kb
    WHERE kb.deleted_at IS NULL
),
doc_eval AS (
    SELECT
        k.id,
        k.tenant_id,
        k.knowledge_base_id,
        k.processing_generation,
        k.parse_status,
        (
            LOWER(COALESCE(k.parse_status, '')) = 'completed'
            AND LOWER(COALESCE(k.summary_status, 'none')) IN ('none', 'completed')
            AND LOWER(COALESCE(k.enrichment_status, 'none')) IN ('none', 'completed')
            AND COALESCE(k.pending_subtasks_count, 0) = 0
            AND (
                (NOT kb.is_hybrid AND LOWER(COALESCE(k.wiki_status, 'none')) IN ('none', 'completed'))
                OR (kb.is_hybrid AND LOWER(COALESCE(k.wiki_status, 'none')) = 'completed')
            )
        ) AS current_complete
    FROM knowledges k
    INNER JOIN live_kb kb ON kb.id = k.knowledge_base_id
    WHERE k.deleted_at IS NULL
),
kb_action AS (
    SELECT
        kb.id,
        kb.type,
        kb.is_hybrid,
        COUNT(d.id) AS document_count,
        COUNT(d.id) FILTER (WHERE NOT d.current_complete) AS noncomplete_documents,
        CASE
            WHEN kb.type <> 'document' THEN 'MANUAL_REVIEW_NON_DOCUMENT'
            WHEN COUNT(d.id) = 0 THEN 'KEEP_EMPTY'
            WHEN COUNT(d.id) FILTER (WHERE NOT d.current_complete) = 0 THEN 'KEEP_ALL_COMPLETE'
            WHEN kb.is_hybrid THEN 'CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST'
            ELSE 'REBUILD_DOCUMENT_KB_IN_PLACE'
        END AS release_action
    FROM live_kb kb
    LEFT JOIN doc_eval d ON d.knowledge_base_id = kb.id
    GROUP BY kb.id, kb.type, kb.is_hybrid
),
pending_source AS (
    SELECT
        p.*,
        COALESCE(
            NULLIF(p.payload->>'knowledge_id', ''),
            CASE WHEN p.scope = 'knowledge' THEN NULLIF(p.scope_id, '') END
        ) AS inferred_knowledge_id,
        COALESCE(
            NULLIF(p.payload->>'knowledge_base_id', ''),
            CASE WHEN p.scope = 'knowledge_base' THEN NULLIF(p.scope_id, '') END
        ) AS inferred_kb_id,
        NULLIF(p.payload->>'processing_generation', '') AS payload_generation
    FROM task_pending_ops p
),
pending_records AS (
    SELECT
        'pending_op'::text AS record_type,
        p.id::text AS record_id,
        p.task_type,
        p.scope,
        p.scope_id,
        COALESCE(d.id, p.inferred_knowledge_id, '') AS knowledge_id,
        COALESCE(d.knowledge_base_id, p.inferred_kb_id, '') AS knowledge_base_id,
        COALESCE(ka.release_action, 'NO_LIVE_KB') AS knowledge_base_action,
        p.op AS source_state,
        p.fail_count AS attempt_count,
        p.enqueued_at AS event_at,
        CASE
            WHEN p.payload_generation IS NULL THEN 'not_recorded'
            WHEN d.id IS NULL THEN 'no_live_document'
            WHEN p.payload_generation = d.processing_generation THEN 'current'
            ELSE 'stale'
        END AS generation_relation,
        CASE
            WHEN p.task_type = 'knowledge:aux_object' AND p.op = 'owned'
                THEN 'OWNERSHIP_LEDGER_KEEP_NO_REPLAY'
            WHEN ka.release_action IN (
                'CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST',
                'REBUILD_DOCUMENT_KB_IN_PLACE'
            ) THEN 'COVERED_BY_KB_REBUILD_DO_NOT_RAW_REPLAY'
            WHEN d.id IS NOT NULL AND d.current_complete
                THEN 'ALREADY_SUCCEEDED_NO_REPLAY'
            WHEN d.id IS NULL AND COALESCE(p.inferred_knowledge_id, '') <> ''
                THEN 'NO_LIVE_DOCUMENT_NO_REPLAY'
            WHEN COALESCE(ka.release_action, 'NO_LIVE_KB') = 'NO_LIVE_KB'
                THEN 'NO_LIVE_TARGET_NO_REPLAY'
            WHEN d.id IS NOT NULL
                THEN 'MANUAL_REPARSE_LIVE_DOCUMENT'
            WHEN p.task_type = 'kb:delete'
                THEN 'DELETION_INTENT_DEFERRED_NO_RELEASE_REPLAY'
            ELSE 'MANUAL_REVIEW_DO_NOT_RAW_REPLAY'
        END AS recovery_action
    FROM pending_source p
    LEFT JOIN doc_eval d ON d.id = p.inferred_knowledge_id
    LEFT JOIN kb_action ka ON ka.id = COALESCE(d.knowledge_base_id, p.inferred_kb_id)
),
dead_source AS (
    SELECT
        dl.*,
        COALESCE(
            NULLIF(dl.payload->>'knowledge_id', ''),
            CASE WHEN dl.scope = 'knowledge' THEN NULLIF(dl.scope_id, '') END,
            NULLIF(dl.related_id, '')
        ) AS inferred_knowledge_id,
        COALESCE(
            NULLIF(dl.payload->>'knowledge_base_id', ''),
            CASE WHEN dl.scope = 'knowledge_base' THEN NULLIF(dl.scope_id, '') END
        ) AS inferred_kb_id,
        NULLIF(dl.payload->>'processing_generation', '') AS payload_generation
    FROM task_dead_letters dl
),
dead_records AS (
    SELECT
        'dead_letter'::text AS record_type,
        dl.id::text AS record_id,
        dl.task_type,
        dl.scope,
        dl.scope_id,
        COALESCE(d.id, dl.inferred_knowledge_id, '') AS knowledge_id,
        COALESCE(d.knowledge_base_id, dl.inferred_kb_id, '') AS knowledge_base_id,
        COALESCE(ka.release_action, 'NO_LIVE_KB') AS knowledge_base_action,
        'failed'::text AS source_state,
        dl.fail_count AS attempt_count,
        dl.failed_at AS event_at,
        CASE
            WHEN dl.payload_generation IS NULL THEN 'not_recorded'
            WHEN d.id IS NULL THEN 'no_live_document'
            WHEN dl.payload_generation = d.processing_generation THEN 'current'
            ELSE 'stale'
        END AS generation_relation,
        CASE
            WHEN ka.release_action IN (
                'CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST',
                'REBUILD_DOCUMENT_KB_IN_PLACE'
            ) THEN 'COVERED_BY_KB_REBUILD_DO_NOT_RAW_REPLAY'
            WHEN d.id IS NOT NULL AND d.current_complete
                THEN 'STALE_OR_ALREADY_SUCCEEDED_NO_REPLAY'
            WHEN d.id IS NULL AND COALESCE(dl.inferred_knowledge_id, '') <> ''
                THEN 'NO_LIVE_DOCUMENT_NO_REPLAY'
            WHEN COALESCE(ka.release_action, 'NO_LIVE_KB') = 'NO_LIVE_KB'
                THEN 'NO_LIVE_TARGET_NO_REPLAY'
            WHEN d.id IS NOT NULL
                THEN 'MANUAL_REPARSE_LIVE_DOCUMENT'
            WHEN dl.task_type = 'kb:delete'
                THEN 'DELETION_INTENT_DEFERRED_NO_RELEASE_REPLAY'
            ELSE 'MANUAL_REVIEW_DO_NOT_RAW_REPLAY'
        END AS recovery_action
    FROM dead_source dl
    LEFT JOIN doc_eval d ON d.id = dl.inferred_knowledge_id
    LEFT JOIN kb_action ka ON ka.id = COALESCE(d.knowledge_base_id, dl.inferred_kb_id)
),
workflow_records AS (
    SELECT
        'document_workflow'::text AS record_type,
        w.id::text AS record_id,
        w.task_type,
        'knowledge'::text AS scope,
        w.knowledge_id AS scope_id,
        w.knowledge_id,
        w.knowledge_base_id,
        COALESCE(ka.release_action, 'NO_LIVE_KB') AS knowledge_base_action,
        (w.state::text || ':' || w.stage)::text AS source_state,
        w.dispatch_attempts AS attempt_count,
        w.updated_at AS event_at,
        CASE
            WHEN d.id IS NULL THEN 'no_live_document'
            WHEN w.processing_generation = d.processing_generation THEN 'current'
            ELSE 'stale'
        END AS generation_relation,
        CASE
            WHEN w.state IN ('completed', 'cancelled', 'superseded')
                THEN 'TERMINAL_NO_REPLAY'
            WHEN ka.release_action IN (
                'CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST',
                'REBUILD_DOCUMENT_KB_IN_PLACE'
            ) THEN 'COVERED_BY_KB_REBUILD_DO_NOT_RAW_REPLAY'
            WHEN d.id IS NOT NULL AND d.current_complete
                THEN 'ALREADY_SUCCEEDED_NO_REPLAY'
            WHEN d.id IS NOT NULL
                THEN 'MANUAL_REPARSE_LIVE_DOCUMENT'
            ELSE 'NO_LIVE_TARGET_NO_REPLAY'
        END AS recovery_action
    FROM custom_document_queue_workflows w
    LEFT JOIN doc_eval d ON d.id = w.knowledge_id
    LEFT JOIN kb_action ka ON ka.id = w.knowledge_base_id
    WHERE w.state NOT IN ('completed', 'cancelled', 'superseded')
       OR w.updated_at >= CURRENT_TIMESTAMP - INTERVAL '30 days'
)
SELECT * FROM pending_records
UNION ALL
SELECT * FROM dead_records
UNION ALL
SELECT * FROM workflow_records
ORDER BY record_type, event_at, record_id;

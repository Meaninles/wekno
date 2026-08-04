-- Per-document rebuild ledger for every live KB. This is stored only in the
-- protected release cutoff directory because it contains titles and source
-- object paths. It complements (and must have the same cutoff as)
-- knowledge-base-inventory.sql.
WITH live_kb AS (
    SELECT
        kb.id,
        kb.tenant_id,
        kb.name,
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
        k.*,
        kb.name AS knowledge_base_name,
        kb.type AS knowledge_base_type,
        kb.is_hybrid,
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
tag_list AS (
    SELECT
        rel.knowledge_id,
        JSONB_AGG(
            JSONB_BUILD_OBJECT(
                'source_tag_id', tag.id,
                'name', tag.name,
                'color', tag.color,
                'sort_order', tag.sort_order
            )
            ORDER BY tag.sort_order, tag.name, tag.id
        ) FILTER (WHERE tag.id IS NOT NULL) AS tags
    FROM knowledge_tag_relations rel
    INNER JOIN knowledge_tags tag
        ON tag.id = rel.tag_id AND tag.deleted_at IS NULL
    GROUP BY rel.knowledge_id
)
SELECT
    d.tenant_id,
    d.knowledge_base_id AS source_knowledge_base_id,
    d.knowledge_base_name AS source_knowledge_base_name,
    d.knowledge_base_type,
    d.is_hybrid,
    ka.release_action AS knowledge_base_action,
    d.id AS source_knowledge_id,
    d.type AS knowledge_type,
    d.channel,
    d.title,
    d.file_name,
    d.file_type,
    d.file_size,
    d.file_hash,
    d.file_path AS source_object_path,
    d.source,
    d.parse_status,
    d.summary_status,
    d.enrichment_status,
    d.wiki_status,
    d.pending_subtasks_count,
    d.enable_status,
    d.folder_id,
    d.processing_generation,
    COALESCE(t.tags, '[]'::JSONB) AS tags,
    d.current_complete,
    CASE
        WHEN ka.release_action IN ('KEEP_ALL_COMPLETE', 'KEEP_EMPTY')
            THEN 'KEEP_NO_REBUILD'
        WHEN ka.release_action = 'REBUILD_DOCUMENT_KB_IN_PLACE'
            THEN 'BATCH_REPARSE_IN_PLACE'
        WHEN ka.release_action = 'CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST'
             AND LOWER(COALESCE(d.parse_status, '')) = 'completed'
            THEN 'CLONE_TO_PURE_DOCUMENT_TARGET_THEN_BATCH_REPARSE'
        WHEN ka.release_action = 'CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST'
             AND NULLIF(d.file_path, '') IS NOT NULL
            THEN 'DOWNLOAD_ORIGINAL_AND_UPLOAD_TO_REPLACEMENT'
        WHEN ka.release_action = 'CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST'
            THEN 'RECREATE_MANUAL_OR_URL_SOURCE_IN_REPLACEMENT'
        ELSE 'MANUAL_REVIEW'
    END AS document_action
FROM doc_eval d
INNER JOIN kb_action ka ON ka.id = d.knowledge_base_id
LEFT JOIN tag_list t ON t.knowledge_id = d.id
ORDER BY d.tenant_id, d.knowledge_base_name, d.knowledge_base_id,
         d.created_at, d.id;

-- Active consumers of KB identities. A replacement KB is not complete until
-- these operational references point at the new target (or are duplicated for
-- share/pin semantics). Historical messages and task payloads are deliberately
-- excluded; they remain immutable audit records and are never replayed.
WITH live_kb AS (
    SELECT
        kb.id,
        kb.tenant_id,
        (
            LOWER(COALESCE(kb.indexing_strategy->>'wiki_enabled', 'false')) = 'true'
            OR EXISTS (
                SELECT 1 FROM wiki_pages wp
                WHERE wp.knowledge_base_id=kb.id AND wp.deleted_at IS NULL
            )
            OR EXISTS (
                SELECT 1 FROM knowledges k
                WHERE k.knowledge_base_id=kb.id AND k.deleted_at IS NULL
                  AND LOWER(COALESCE(k.wiki_status, 'none')) <> 'none'
            )
        ) AS is_hybrid
    FROM knowledge_bases kb
    WHERE kb.deleted_at IS NULL
), document_eval AS (
    SELECT
        k.knowledge_base_id,
        (
            LOWER(COALESCE(k.parse_status, '')) = 'completed'
            AND LOWER(COALESCE(k.summary_status, 'none')) IN ('none', 'completed')
            AND LOWER(COALESCE(k.enrichment_status, 'none')) IN ('none', 'completed')
            AND COALESCE(k.pending_subtasks_count, 0) = 0
            AND (
                (NOT kb.is_hybrid AND LOWER(COALESCE(k.wiki_status, 'none')) IN ('none', 'completed'))
                OR (kb.is_hybrid AND LOWER(COALESCE(k.wiki_status, 'none')) = 'completed')
            )
        ) AS complete
    FROM knowledges k
    INNER JOIN live_kb kb ON kb.id=k.knowledge_base_id
    WHERE k.deleted_at IS NULL
), replacement AS MATERIALIZED (
    SELECT kb.id, kb.tenant_id
    FROM live_kb kb
    LEFT JOIN document_eval d ON d.knowledge_base_id=kb.id
    GROUP BY kb.id, kb.tenant_id, kb.is_hybrid
    HAVING kb.is_hybrid
       AND COUNT(d.*) > 0
       AND COUNT(d.*) FILTER (WHERE NOT d.complete) > 0
), records AS (
    SELECT
        'CUSTOM_AGENT'::text AS reference_type,
        a.tenant_id::text AS tenant_id,
        x.kb_id AS source_knowledge_base_id,
        a.id AS reference_id,
        ''::text AS reference_sub_id,
        'REPLACE_IN_CONFIG'::text AS required_action
    FROM custom_agents a
    CROSS JOIN LATERAL JSONB_ARRAY_ELEMENTS_TEXT(
        CASE WHEN JSONB_TYPEOF(a.config->'knowledge_bases')='array'
             THEN a.config->'knowledge_bases' ELSE '[]'::JSONB END
    ) x(kb_id)
    WHERE a.deleted_at IS NULL AND x.kb_id IN (SELECT id FROM replacement)

    UNION ALL
    SELECT
        'SCHEDULED_CHAT', t.tenant_id::text, x.kb_id, t.id, '', 'REPLACE_IN_REQUEST_CONTEXT'
    FROM custom_scheduled_chat_tasks t
    CROSS JOIN LATERAL JSONB_ARRAY_ELEMENTS_TEXT(
        CASE WHEN JSONB_TYPEOF(t.request_context->'knowledge_base_ids')='array'
             THEN t.request_context->'knowledge_base_ids' ELSE '[]'::JSONB END
    ) x(kb_id)
    WHERE t.deleted_at IS NULL AND x.kb_id IN (SELECT id FROM replacement)

    UNION ALL
    SELECT
        'KB_SHARE', s.source_tenant_id::text, s.knowledge_base_id, s.id,
        s.organization_id, 'DUPLICATE_TO_TARGET:' || s.permission
    FROM kb_shares s
    WHERE s.deleted_at IS NULL AND s.knowledge_base_id IN (SELECT id FROM replacement)

    UNION ALL
    SELECT
        'USER_PIN', p.tenant_id::text, p.kb_id, p.user_id, '', 'DUPLICATE_TO_TARGET'
    FROM user_kb_pins p
    WHERE p.kb_id IN (SELECT id FROM replacement)

    UNION ALL
    SELECT
        'DATA_SOURCE', d.tenant_id::text, d.knowledge_base_id, d.id, d.type,
        'RECREATE_OR_REBIND_TO_TARGET'
    FROM data_sources d
    WHERE d.deleted_at IS NULL AND d.knowledge_base_id IN (SELECT id FROM replacement)

    UNION ALL
    SELECT
        'IM_CHANNEL', c.tenant_id::text, c.knowledge_base_id, c.id, c.platform,
        'REBIND_TO_TARGET'
    FROM im_channels c
    WHERE c.deleted_at IS NULL AND c.knowledge_base_id IN (SELECT id FROM replacement)

    UNION ALL
    SELECT
        'SESSION', s.tenant_id::text, s.knowledge_base_id, s.id, '',
        'REBIND_LIVE_SESSION'
    FROM sessions s
    WHERE s.deleted_at IS NULL AND s.knowledge_base_id IN (SELECT id FROM replacement)
)
SELECT * FROM records
ORDER BY source_knowledge_base_id, reference_type, reference_id, reference_sub_id;

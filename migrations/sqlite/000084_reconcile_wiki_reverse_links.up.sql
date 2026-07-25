-- SQLite equivalent of the set-based reverse wiki-link reconciliation.
WITH reverse_links AS (
    SELECT
        expanded.tenant_id,
        expanded.knowledge_base_id,
        expanded.target_slug,
        json_group_array(expanded.source_slug) AS in_links
    FROM (
        SELECT DISTINCT
            source.tenant_id,
            source.knowledge_base_id,
            source.slug AS source_slug,
            CAST(edge.value AS TEXT) AS target_slug
        FROM wiki_pages AS source
        JOIN json_each(
            CASE
                WHEN json_valid(source.out_links)
                 AND json_type(source.out_links) = 'array'
                    THEN source.out_links
                ELSE '[]'
            END
        ) AS edge
        WHERE source.deleted_at IS NULL
          AND source.status <> 'archived'
        ORDER BY
            source.tenant_id,
            source.knowledge_base_id,
            target_slug,
            source_slug
    ) AS expanded
    GROUP BY
        expanded.tenant_id,
        expanded.knowledge_base_id,
        expanded.target_slug
)
UPDATE wiki_pages AS target
SET in_links = COALESCE(
    (
        SELECT reverse_links.in_links
        FROM reverse_links
        WHERE reverse_links.tenant_id = target.tenant_id
          AND reverse_links.knowledge_base_id = target.knowledge_base_id
          AND reverse_links.target_slug = target.slug
    ),
    '[]'
)
WHERE target.deleted_at IS NULL;

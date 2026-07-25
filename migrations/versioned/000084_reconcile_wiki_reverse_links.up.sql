-- Rebuild reverse wiki edges from their authoritative forward edges.
--
-- Older application versions updated a target page's entire in_links array
-- through optimistic locking. Concurrent source pages could therefore leave a
-- missing reverse edge even though the corresponding out_link was committed.
-- Expand and aggregate all live forward edges once, then update only targets
-- whose derived reverse-link set differs.
WITH reverse_links AS (
    SELECT
        expanded.tenant_id,
        expanded.knowledge_base_id,
        expanded.target_slug,
        jsonb_agg(expanded.source_slug ORDER BY expanded.source_slug) AS in_links
    FROM (
        SELECT DISTINCT
            source.tenant_id,
            source.knowledge_base_id,
            source.slug AS source_slug,
            edge.target_slug
        FROM wiki_pages AS source
        CROSS JOIN LATERAL jsonb_array_elements_text(
            CASE
                WHEN jsonb_typeof(source.out_links) = 'array'
                    THEN source.out_links
                ELSE '[]'::jsonb
            END
        ) AS edge(target_slug)
        WHERE source.deleted_at IS NULL
          AND source.status <> 'archived'
    ) AS expanded
    GROUP BY
        expanded.tenant_id,
        expanded.knowledge_base_id,
        expanded.target_slug
),
reconciled AS (
    SELECT
        target.id,
        COALESCE(reverse_links.in_links, '[]'::jsonb) AS in_links
    FROM wiki_pages AS target
    LEFT JOIN reverse_links
      ON reverse_links.tenant_id = target.tenant_id
     AND reverse_links.knowledge_base_id = target.knowledge_base_id
     AND reverse_links.target_slug = target.slug
    WHERE target.deleted_at IS NULL
)
UPDATE wiki_pages AS target
SET in_links = reconciled.in_links
FROM reconciled
WHERE target.id = reconciled.id
  AND target.in_links IS DISTINCT FROM reconciled.in_links;

-- Migration: 000066_ensure_knowledge_tag_relations
-- Description: Repair databases that already consumed the former local
-- 000063 migration before upstream introduced multi-tag relations at 000063.
DO $$ BEGIN RAISE NOTICE '[Migration 000066] Ensuring knowledge_tag_relations exists...'; END $$;

CREATE TABLE IF NOT EXISTS knowledge_tag_relations (
    knowledge_id VARCHAR(36) NOT NULL,
    tag_id        VARCHAR(36) NOT NULL,
    created_at    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (knowledge_id, tag_id)
);

CREATE INDEX IF NOT EXISTS idx_ktr_knowledge
    ON knowledge_tag_relations(knowledge_id);

CREATE INDEX IF NOT EXISTS idx_ktr_tag
    ON knowledge_tag_relations(tag_id);

-- Older local databases may still have knowledges.tag_id because their
-- migration version 63 was the local drop-users-email migration. Add the
-- column only when absent so the SELECT below is always valid, copy any
-- legacy single-tag data, then remove the legacy column.
ALTER TABLE knowledges ADD COLUMN IF NOT EXISTS tag_id VARCHAR(36);

INSERT INTO knowledge_tag_relations (knowledge_id, tag_id, created_at)
SELECT id, tag_id, COALESCE(updated_at, NOW())
FROM knowledges
WHERE tag_id IS NOT NULL AND tag_id != ''
  AND deleted_at IS NULL
ON CONFLICT (knowledge_id, tag_id) DO NOTHING;

DROP INDEX IF EXISTS idx_knowledges_tag;
ALTER TABLE knowledges DROP COLUMN IF EXISTS tag_id;

DO $$ BEGIN RAISE NOTICE '[Migration 000066] knowledge_tag_relations ensured'; END $$;

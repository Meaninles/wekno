-- Migration: 000066_ensure_knowledge_tag_relations (rollback)
-- Intentionally no-op. The canonical rollback for knowledge_tag_relations
-- belongs to 000063_knowledge_multi_tags.down.sql; dropping it here would
-- make multi-step rollbacks through 000063 fail.
DO $$ BEGIN RAISE NOTICE '[Migration 000066 rollback] no-op'; END $$;

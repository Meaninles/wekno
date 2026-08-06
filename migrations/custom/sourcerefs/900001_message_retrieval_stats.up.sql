-- Source-reference extension schema contract. The executable production and
-- SQLite migration mirrors live in migrations/versioned/000095 and
-- migrations/sqlite/000095 because the native migrator consumes those paths.
ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS retrieval_stats JSONB NOT NULL DEFAULT '{}'::jsonb;

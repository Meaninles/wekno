ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS retrieval_stats JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN messages.retrieval_stats IS
    'Authoritative unique source counts inspected during this answer; independent from cited references';

-- Custom migration: built-in agent retrieval defaults.
--
-- New defaults:
-- - table-analysis uses embedding_top_k=10 and rerank_top_k=5.
-- - every other built-in agent that had the old 10/10 default keeps
--   embedding_top_k=10 and changes rerank_top_k to 5.

UPDATE custom_agents
SET config = jsonb_set(jsonb_set(config, '{embedding_top_k}', '10'::jsonb, true), '{rerank_top_k}', '5'::jsonb, true),
    updated_at = NOW()
WHERE is_builtin = TRUE
  AND id = 'builtin-table-analyst'
  AND (
    (config->>'embedding_top_k' = '5' AND config->>'rerank_top_k' = '5')
    OR (config->>'embedding_top_k' = '10' AND config->>'rerank_top_k' = '10')
    OR config->>'embedding_top_k' IS NULL
  );

UPDATE custom_agents
SET config = jsonb_set(config, '{rerank_top_k}', '5'::jsonb, true),
    updated_at = NOW()
WHERE is_builtin = TRUE
  AND id <> 'builtin-table-analyst'
  AND config->>'embedding_top_k' = '10'
  AND config->>'rerank_top_k' = '10';

-- A post-process receipt proves that every optional derivative and Wiki
-- intent for the exact generation was durably published. Older runtimes kept
-- such rows in finalizing until those optional branches terminated; the core
-- lifecycle now closes immediately after the receipt.
UPDATE knowledges
SET parse_status = 'completed',
    processing_owner = '',
    processing_fanout = NULL,
    processed_at = COALESCE(processed_at, CURRENT_TIMESTAMP),
    updated_at = CURRENT_TIMESTAMP
WHERE parse_status = 'finalizing'
  AND EXISTS (
      SELECT 1
      FROM knowledge_fanout_completions AS completion
      WHERE completion.tenant_id = knowledges.tenant_id
        AND completion.knowledge_id = knowledges.id
        AND completion.knowledge_base_id = knowledges.knowledge_base_id
        AND completion.processing_generation = knowledges.processing_generation
        AND completion.item_id = 'orchestration:postprocess'
  );

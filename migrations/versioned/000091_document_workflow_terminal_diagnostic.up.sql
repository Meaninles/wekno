-- A terminal workflow must carry its own durable outcome. Processing spans are
-- compactable diagnostics and cannot be the only source of terminal truth.
ALTER TABLE custom_document_queue_workflows
    ADD COLUMN IF NOT EXISTS terminal_diagnostic jsonb NULL;

-- Production upgrades from v90 already contain terminal rows, including
-- cancelled rows whose completed_at is NULL. Backfill from state rather than
-- timestamps so every historical terminal row satisfies the invariant.
UPDATE custom_document_queue_workflows
SET terminal_diagnostic = jsonb_build_object(
        'source', 'workflow',
        'status', CASE state
            WHEN 'completed' THEN 'done'
            WHEN 'failed' THEN 'failed'
            WHEN 'cancelled' THEN 'cancelled'
            WHEN 'superseded' THEN 'cancelled'
        END,
        'error_code', CASE state
            WHEN 'completed' THEN ''
            WHEN 'failed' THEN 'DOCUMENT_WORKFLOW_FAILED'
            ELSE 'DOCUMENT_WORKFLOW_CANCELLED'
        END,
        'error_message', CASE
            WHEN state = 'completed' THEN ''
            ELSE btrim(COALESCE(last_error, ''))
        END
    )
WHERE state IN ('completed', 'failed', 'cancelled', 'superseded');

ALTER TABLE custom_document_queue_workflows
    DROP CONSTRAINT IF EXISTS ck_document_workflow_terminal_diagnostic;

ALTER TABLE custom_document_queue_workflows
    ADD CONSTRAINT ck_document_workflow_terminal_diagnostic
    CHECK (
        state NOT IN ('completed', 'failed', 'cancelled', 'superseded')
        OR (
            terminal_diagnostic IS NOT NULL
            AND jsonb_typeof(terminal_diagnostic) = 'object'
            AND terminal_diagnostic ->> 'source' = 'workflow'
            AND terminal_diagnostic ->> 'status' = CASE state
                WHEN 'completed' THEN 'done'
                WHEN 'failed' THEN 'failed'
                WHEN 'cancelled' THEN 'cancelled'
                WHEN 'superseded' THEN 'cancelled'
            END
        )
    );

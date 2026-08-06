ALTER TABLE custom_derivative_work_items
    ADD COLUMN IF NOT EXISTS dispatch_lease_until timestamptz;

CREATE INDEX IF NOT EXISTS idx_derivative_dispatch_lease
    ON custom_derivative_work_items (dispatch_lease_until);

CREATE INDEX IF NOT EXISTS idx_derivative_pool_active_window
    ON custom_derivative_work_items (resource_pool_id, state, dispatch_lease_until)
    WHERE work_kind <> 'finalizer';

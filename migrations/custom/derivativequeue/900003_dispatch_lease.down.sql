DROP INDEX IF EXISTS idx_derivative_pool_active_window;
DROP INDEX IF EXISTS idx_derivative_dispatch_lease;
ALTER TABLE custom_derivative_work_items
    DROP COLUMN IF EXISTS dispatch_lease_until;

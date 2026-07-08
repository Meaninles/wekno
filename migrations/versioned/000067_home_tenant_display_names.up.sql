-- Migration: 000067_home_tenant_display_names
-- Description: Backfill generated home tenant names from user display names.

UPDATE tenants AS t
SET
    name = (
        COALESCE(NULLIF(BTRIM(u.display_name), ''), NULLIF(BTRIM(u.username), ''), 'User')
        || '''s Workspace'
    ),
    updated_at = NOW()
FROM users AS u
WHERE u.tenant_id = t.id
  AND u.deleted_at IS NULL
  AND t.deleted_at IS NULL
  AND (
      COALESCE(BTRIM(t.name), '') = ''
      OR BTRIM(t.name) LIKE '%''s Workspace'
  )
  AND t.name IS DISTINCT FROM (
      COALESCE(NULLIF(BTRIM(u.display_name), ''), NULLIF(BTRIM(u.username), ''), 'User')
      || '''s Workspace'
  );

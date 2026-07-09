-- Rollback: drop users.last_login_at.

DO $$ BEGIN RAISE NOTICE '[Migration 000068 DOWN] Dropping users.last_login_at column...'; END $$;

ALTER TABLE users
    DROP COLUMN IF EXISTS last_login_at;

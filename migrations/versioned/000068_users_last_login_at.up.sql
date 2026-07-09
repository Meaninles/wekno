-- Migration: 000068_users_last_login_at
-- Adds users.last_login_at for recording the latest successful login time.

DO $$ BEGIN RAISE NOTICE '[Migration 000068] Adding users.last_login_at column...'; END $$;

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMP WITH TIME ZONE;

COMMENT ON COLUMN users.last_login_at IS 'Most recent successful login time of the user';

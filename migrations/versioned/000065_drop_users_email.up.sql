DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'users'
      AND column_name = 'email'
  ) THEN
    UPDATE users
    SET username = email
    WHERE email IS NOT NULL
      AND email <> ''
      AND username IS DISTINCT FROM email;
  END IF;
END $$;

DROP INDEX IF EXISTS idx_users_email;

ALTER TABLE users
  DROP CONSTRAINT IF EXISTS users_email_key;

ALTER TABLE users
  DROP COLUMN IF EXISTS email;

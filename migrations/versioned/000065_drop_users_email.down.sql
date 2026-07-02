ALTER TABLE users
  ADD COLUMN IF NOT EXISTS email VARCHAR(255);

UPDATE users
SET email = username
WHERE email IS NULL OR email = '';

ALTER TABLE users
  ALTER COLUMN email SET NOT NULL;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conrelid = 'users'::regclass
      AND conname = 'users_email_key'
  ) THEN
    ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);

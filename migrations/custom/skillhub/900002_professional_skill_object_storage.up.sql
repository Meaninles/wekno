ALTER TABLE custom_professional_skills
  ADD COLUMN IF NOT EXISTS display_name varchar(255) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS object_path text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS object_size bigint NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS object_sha256 varchar(64) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS file_count integer NOT NULL DEFAULT 0;

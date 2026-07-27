ALTER TABLE custom_professional_skills
  DROP COLUMN IF EXISTS file_count,
  DROP COLUMN IF EXISTS object_sha256,
  DROP COLUMN IF EXISTS object_size,
  DROP COLUMN IF EXISTS object_path,
  DROP COLUMN IF EXISTS display_name;

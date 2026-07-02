CREATE TABLE IF NOT EXISTS custom_skills (
  id varchar(36) PRIMARY KEY,
  tenant_id bigint NOT NULL,
  creator_id varchar(36) NOT NULL,
  name varchar(64) NOT NULL,
  description text NOT NULL,
  instructions text NOT NULL,
  enabled boolean NOT NULL DEFAULT true,
  created_at timestamp with time zone,
  updated_at timestamp with time zone,
  deleted_at timestamp with time zone
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_skill_global_name
  ON custom_skills (name)
  WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_skill_tenant_name
  ON custom_skills (tenant_id, name)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_custom_skills_tenant_id ON custom_skills (tenant_id);
CREATE INDEX IF NOT EXISTS idx_custom_skills_creator_id ON custom_skills (creator_id);
CREATE INDEX IF NOT EXISTS idx_custom_skills_deleted_at ON custom_skills (deleted_at);

CREATE TABLE IF NOT EXISTS custom_professional_skills (
  id varchar(36) PRIMARY KEY,
  tenant_id bigint NOT NULL,
  creator_id varchar(36) NOT NULL,
  name varchar(64) NOT NULL,
  description text NOT NULL,
  archive_file_name varchar(255),
  created_at timestamp with time zone,
  updated_at timestamp with time zone,
  deleted_at timestamp with time zone
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_professional_skill_name
  ON custom_professional_skills (name)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_custom_professional_skills_tenant_id ON custom_professional_skills (tenant_id);
CREATE INDEX IF NOT EXISTS idx_custom_professional_skills_creator_id ON custom_professional_skills (creator_id);
CREATE INDEX IF NOT EXISTS idx_custom_professional_skills_deleted_at ON custom_professional_skills (deleted_at);

CREATE TABLE IF NOT EXISTS custom_skill_org_shares (
  id varchar(36) PRIMARY KEY,
  skill_id varchar(36) NOT NULL,
  organization_id varchar(36) NOT NULL,
  shared_by_user_id varchar(36) NOT NULL,
  source_tenant_id bigint NOT NULL,
  permission varchar(32) NOT NULL DEFAULT 'viewer',
  created_at timestamp with time zone,
  updated_at timestamp with time zone,
  deleted_at timestamp with time zone
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_skill_org_share
  ON custom_skill_org_shares (skill_id, organization_id)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_custom_skill_org_shares_skill_id ON custom_skill_org_shares (skill_id);
CREATE INDEX IF NOT EXISTS idx_custom_skill_org_shares_organization_id ON custom_skill_org_shares (organization_id);
CREATE INDEX IF NOT EXISTS idx_custom_skill_org_shares_source_tenant_id ON custom_skill_org_shares (source_tenant_id);
CREATE INDEX IF NOT EXISTS idx_custom_skill_org_shares_deleted_at ON custom_skill_org_shares (deleted_at);

CREATE TABLE IF NOT EXISTS custom_skill_user_shares (
  id varchar(36) PRIMARY KEY,
  skill_id varchar(36) NOT NULL,
  target_user_id varchar(36) NOT NULL,
  shared_by_user_id varchar(36) NOT NULL,
  source_tenant_id bigint NOT NULL,
  permission varchar(32) NOT NULL DEFAULT 'viewer',
  created_at timestamp with time zone,
  updated_at timestamp with time zone,
  deleted_at timestamp with time zone
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_skill_user_share
  ON custom_skill_user_shares (skill_id, target_user_id)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_custom_skill_user_shares_skill_id ON custom_skill_user_shares (skill_id);
CREATE INDEX IF NOT EXISTS idx_custom_skill_user_shares_target_user_id ON custom_skill_user_shares (target_user_id);
CREATE INDEX IF NOT EXISTS idx_custom_skill_user_shares_source_tenant_id ON custom_skill_user_shares (source_tenant_id);
CREATE INDEX IF NOT EXISTS idx_custom_skill_user_shares_deleted_at ON custom_skill_user_shares (deleted_at);

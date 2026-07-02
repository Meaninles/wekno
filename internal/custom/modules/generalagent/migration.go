package generalagent

import (
	"context"

	"gorm.io/gorm"
)

var generalAgentMigrationStatements = []string{
	`CREATE TABLE IF NOT EXISTS custom_general_agent_artifacts (
		id VARCHAR(36) PRIMARY KEY,
		tenant_id BIGINT NOT NULL,
		user_id VARCHAR(128) NOT NULL,
		run_id VARCHAR(80) NOT NULL,
		session_id VARCHAR(36) NOT NULL,
		message_id VARCHAR(36),
		file_token VARCHAR(255) NOT NULL,
		file_path TEXT NOT NULL,
		file_name VARCHAR(255) NOT NULL,
		file_type VARCHAR(32) NOT NULL,
		file_size BIGINT NOT NULL DEFAULT 0,
		sha256 VARCHAR(64) NOT NULL,
		content_type VARCHAR(128),
		created_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ,
		deleted_at TIMESTAMPTZ
	)`,
	`ALTER TABLE custom_general_agent_artifacts ALTER COLUMN user_id TYPE VARCHAR(128)`,
	`ALTER TABLE custom_general_agent_artifacts ADD COLUMN IF NOT EXISTS file_token VARCHAR(255)`,
	`UPDATE custom_general_agent_artifacts SET file_token = id WHERE file_token IS NULL OR file_token = ''`,
	`ALTER TABLE custom_general_agent_artifacts ALTER COLUMN file_token SET NOT NULL`,
	`CREATE INDEX IF NOT EXISTS idx_custom_general_agent_artifacts_tenant_id ON custom_general_agent_artifacts (tenant_id)`,
	`CREATE INDEX IF NOT EXISTS idx_custom_general_agent_artifacts_user_id ON custom_general_agent_artifacts (user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_custom_general_agent_artifacts_run_id ON custom_general_agent_artifacts (run_id)`,
	`CREATE INDEX IF NOT EXISTS idx_custom_general_agent_artifacts_session_id ON custom_general_agent_artifacts (session_id)`,
	`CREATE INDEX IF NOT EXISTS idx_custom_general_agent_artifacts_deleted_at ON custom_general_agent_artifacts (deleted_at)`,
	`UPDATE custom_agents
		SET config = config - 'allowed_artifact_formats' - 'max_artifacts',
		updated_at = NOW()
		WHERE config->>'agent_type' = 'general-agent'
		  AND (config->'allowed_artifact_formats' IS NOT NULL OR config->'max_artifacts' IS NOT NULL)`,
	`DELETE FROM mcp_services
		WHERE tenant_id = 0
		  AND name = 'General Agent MCP Fixtures'
		  AND is_builtin = TRUE`,
}

func applyGeneralAgentMigrations(ctx context.Context, db *gorm.DB) error {
	for _, stmt := range generalAgentMigrationStatements {
		if err := db.WithContext(ctx).Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

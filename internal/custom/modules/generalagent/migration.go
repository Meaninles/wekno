package generalagent

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
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
}

func applyGeneralAgentMigrations(ctx context.Context, db *gorm.DB) error {
	for _, stmt := range generalAgentMigrationStatements {
		if err := db.WithContext(ctx).Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ensureDevFixtureMCP(ctx context.Context) error {
	if !truthyEnv("CUSTOM_GENERAL_AGENT_REGISTER_MCP_FIXTURES") {
		return nil
	}
	name := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_MCP_FIXTURE_NAME"))
	if name == "" {
		name = "General Agent MCP Fixtures"
	}
	url := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_MCP_FIXTURE_URL"))
	if url == "" {
		url = "http://weknora-custom-mcp-fixtures:8092/mcp"
	}
	now := time.Now()
	var existing types.MCPService
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ?", uint64(0), name).
		First(&existing).Error
	if err == nil {
		existing.Description = "Development MCP fixture service for validating general-agent MCP selection and tool bridge."
		existing.Enabled = true
		existing.TransportType = types.MCPTransportHTTPStreamable
		existing.URL = &url
		existing.Headers = types.MCPHeaders{}
		existing.AuthConfig = &types.MCPAuthConfig{AuthType: types.MCPAuthNone}
		existing.AdvancedConfig = &types.MCPAdvancedConfig{Timeout: 30, RetryCount: 1, RetryDelay: 1}
		existing.IsBuiltin = true
		existing.UpdatedAt = now
		if saveErr := s.db.WithContext(ctx).Save(&existing).Error; saveErr != nil {
			return saveErr
		}
		logger.Infof(ctx, "general-agent dev MCP fixture service updated: %s", existing.ID)
		return nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	svc := &types.MCPService{
		TenantID:       0,
		Name:           name,
		Description:    "Development MCP fixture service for validating general-agent MCP selection and tool bridge.",
		Enabled:        true,
		TransportType:  types.MCPTransportHTTPStreamable,
		URL:            &url,
		Headers:        types.MCPHeaders{},
		AuthConfig:     &types.MCPAuthConfig{AuthType: types.MCPAuthNone},
		AdvancedConfig: &types.MCPAdvancedConfig{Timeout: 30, RetryCount: 1, RetryDelay: 1},
		IsBuiltin:      true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if createErr := s.db.WithContext(ctx).Create(svc).Error; createErr != nil {
		return createErr
	}
	logger.Infof(ctx, "general-agent dev MCP fixture service registered: %s", svc.ID)
	return nil
}

func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

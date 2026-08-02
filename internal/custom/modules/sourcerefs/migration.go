package sourcerefs

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// Migrate installs the durable retrieval summary used by every chat runtime.
// It is called only by the elected maintenance role, so API replicas never
// race to mutate the shared message table during startup.
func Migrate(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("source reference database is unavailable")
	}
	columns := []struct {
		field    string
		postgres string
		sqlite   string
	}{
		{"RetrievalStats", `ALTER TABLE messages ADD COLUMN IF NOT EXISTS retrieval_stats JSONB NOT NULL DEFAULT '{}'::jsonb`, `ALTER TABLE messages ADD COLUMN retrieval_stats TEXT NOT NULL DEFAULT '{}'`},
		{"AgentMode", `ALTER TABLE messages ADD COLUMN IF NOT EXISTS agent_mode BOOLEAN NOT NULL DEFAULT FALSE`, `ALTER TABLE messages ADD COLUMN agent_mode INTEGER NOT NULL DEFAULT 0`},
		{"AgentToolCount", `ALTER TABLE messages ADD COLUMN IF NOT EXISTS agent_tool_count INTEGER NOT NULL DEFAULT 0`, `ALTER TABLE messages ADD COLUMN agent_tool_count INTEGER NOT NULL DEFAULT 0`},
	}
	for _, column := range columns {
		if db.Migrator().HasColumn(&types.Message{}, column.field) {
			continue
		}
		statement := column.sqlite
		if db.Dialector.Name() == "postgres" {
			statement = column.postgres
		} else if db.Dialector.Name() != "sqlite" {
			return fmt.Errorf("unsupported database dialect %q for source reference migration", db.Dialector.Name())
		}
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("migrate message source metadata %s: %w", column.field, err)
		}
	}
	return migrateStoredAgentCitationPrompts(ctx, db)
}

// migrateStoredAgentCitationPrompts replaces the citation section copied into
// agent rows by older deployments with the single current output contract.
// Only system prompts that actually contain the retired citation syntax are
// touched; all other agent instructions and settings remain byte-for-byte
// unchanged.
func migrateStoredAgentCitationPrompts(ctx context.Context, db *gorm.DB) error {
	var agents []types.CustomAgent
	if err := db.WithContext(ctx).Unscoped().
		Select("id", "tenant_id", "config").
		Find(&agents).Error; err != nil {
		return fmt.Errorf("load agent citation prompts: %w", err)
	}
	for i := range agents {
		agent := &agents[i]
		prompt := agent.Config.SystemPrompt
		if !strings.Contains(prompt, "<kb ") && !strings.Contains(prompt, "<web ") {
			continue
		}
		agent.Config.SystemPrompt = EnsureGenerationContract(prompt)
		if err := db.WithContext(ctx).Unscoped().
			Model(&types.CustomAgent{}).
			Where("id = ? AND tenant_id = ?", agent.ID, agent.TenantID).
			Update("config", agent.Config).Error; err != nil {
			return fmt.Errorf("migrate agent %s citation prompt: %w", agent.ID, err)
		}
	}
	return nil
}

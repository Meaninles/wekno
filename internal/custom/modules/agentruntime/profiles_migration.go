package agentruntime

import (
	"context"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// All profiles use one harness without merging their tools or bindings.
func migrateAgentProfiles(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(2070915)`).Error; err != nil {
			return err
		}
		if err := tx.Exec(`CREATE TABLE IF NOT EXISTS custom_agent_profile_cutovers (version integer PRIMARY KEY)`).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Table("custom_agent_profile_cutovers").Where("version = 2").Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return nil
		}
		var agents []types.CustomAgent
		if err := tx.Find(&agents).Error; err != nil {
			return err
		}
		for _, agent := range agents {
			if agent.ID == "builtin-quick-answer" {
				continue
			}
			oldID := agent.ID
			if oldID == "builtin-smart-reasoning" {
				agent.ID = types.BuiltinKnowledgeQAID
			}
			if agent.IsBuiltin {
				if defaults := types.GetBuiltinAgent(agent.ID, agent.TenantID); defaults != nil {
					agent.Name, agent.Description, agent.Avatar = defaults.Name, defaults.Description, defaults.Avatar
					agent.Config.AgentType = defaults.Config.AgentType
				}
			} else if agent.Config.AgentType == "rag-qa" || agent.Config.AgentMode == "quick-answer" {
				agent.Config.AgentType = types.AgentTypeKnowledgeQA
			}
			switch agent.Config.SystemPromptID {
			case "general_claude_agent":
				agent.Config.SystemPromptID = "general_agent"
			case "document_claude_agent":
				agent.Config.SystemPromptID = "document_agent"
			}
			agent.EnsureDefaults()
			if err := tx.Save(&agent).Error; err != nil {
				return err
			}
			if oldID != agent.ID {
				if err := tx.Unscoped().Where("id = ? AND tenant_id = ?", oldID, agent.TenantID).Delete(&types.CustomAgent{}).Error; err != nil {
					return err
				}
			}
		}
		for _, old := range []string{"builtin-quick-answer", "builtin-smart-reasoning"} {
			for _, table := range []string{"sessions", "im_channels", "embed_channels", "custom_scheduled_chat_tasks"} {
				if !tx.Migrator().HasTable(table) {
					continue
				}
				if err := tx.Table(table).Where("agent_id = ?", old).Update("agent_id", types.BuiltinKnowledgeQAID).Error; err != nil {
					return err
				}
			}
			if tx.Migrator().HasTable("sessions") {
				if err := tx.Exec(`UPDATE sessions SET agent_config = jsonb_set(agent_config::jsonb, '{agent_id}', to_jsonb(?::text)) WHERE agent_config::jsonb->>'agent_id' = ?`, types.BuiltinKnowledgeQAID, old).Error; err != nil {
					return err
				}
			}
			if err := tx.Unscoped().Where("id = ? AND is_builtin = true", old).Delete(&types.CustomAgent{}).Error; err != nil {
				return err
			}
		}
		return tx.Exec(`INSERT INTO custom_agent_profile_cutovers(version) VALUES (2)`).Error
	})
}

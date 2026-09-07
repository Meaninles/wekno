package agentruntime

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// Remove the retired profile, retaining conversation history and moving active
// bindings to the general agent. This is a one-time data migration, not a runtime alias.
func retireSimpleChat(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(2070915)`).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Table("custom_agent_profile_cutovers").Where("version = 3").Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return nil
		}
		const retired = "builtin-simple-chat"
		for _, table := range []string{"sessions", "im_channels", "im_channel_sessions", "embed_channels", "custom_scheduled_chat_tasks", "custom_browser_agent_system_profiles", "custom_browser_agent_scenarios"} {
			if !tx.Migrator().HasTable(table) {
				continue
			}
			if err := tx.Table(table).Where("agent_id = ?", retired).Update("agent_id", types.BuiltinGeneralAgentID).Error; err != nil {
				return err
			}
		}
		for _, binding := range []struct{ table, column string }{
			{"sessions", "agent_config"}, {"tenants", "agent_config"}, {"custom_scheduled_chat_tasks", "request_context"},
		} {
			if !tx.Migrator().HasColumn(binding.table, binding.column) {
				continue
			}
			// Replace only exact JSON string values in active configuration.
			query := `UPDATE ` + binding.table + ` SET ` + binding.column + ` = replace(` + binding.column + `::text, ?, ?)::jsonb WHERE ` + binding.column + `::text LIKE ?`
			if err := tx.Exec(query, `"`+retired+`"`, `"`+types.BuiltinGeneralAgentID+`"`, `%"`+retired+`"%`).Error; err != nil {
				return err
			}
		}
		for _, table := range []string{"agent_shares", "tenant_disabled_shared_agents", "custom_db_agent_bindings"} {
			if tx.Migrator().HasTable(table) {
				if err := tx.Exec(`DELETE FROM `+table+` WHERE agent_id = ?`, retired).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Unscoped().Where("id = ? AND is_builtin = true", retired).Delete(&types.CustomAgent{}).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO custom_agent_profile_cutovers(version) VALUES (3)`).Error
	})
}

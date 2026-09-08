package builtinagentdefaults

import (
	"context"

	"gorm.io/gorm"
)

// builtinAgentDefaultMigrations update only legacy built-in defaults. Tenant
// configurations with non-default values are preserved. These statements run
// in the dedicated migration role, never on a serving request path.
var builtinAgentDefaultMigrations = []string{
	`UPDATE custom_agents
		SET config = jsonb_set(config, '{temperature}', '0.1'::jsonb, true),
		    updated_at = NOW()
		WHERE id = 'builtin-knowledge-qa'
		  AND is_builtin = TRUE
		  AND config->>'agent_type' = 'knowledge-qa'
		  AND config->>'temperature' IN ('0.7', '0.70')`,
	`UPDATE custom_agents
		SET config = jsonb_set(config, '{history_turns}', '10'::jsonb, true),
		    updated_at = NOW()
		WHERE id = 'builtin-knowledge-qa'
		  AND is_builtin = TRUE
		  AND config->>'agent_type' = 'knowledge-qa'
		  AND config->>'history_turns' = '5'`,
	`UPDATE custom_agents
		SET config = jsonb_set(config, '{web_search_enabled}', 'false'::jsonb, true),
		    updated_at = NOW()
		WHERE id = 'builtin-document-processing'
		  AND is_builtin = TRUE
		  AND config->>'agent_type' = 'document-processing-agent'
		  AND config->>'web_search_enabled' = 'true'`,
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, statement := range builtinAgentDefaultMigrations {
			if err := tx.Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

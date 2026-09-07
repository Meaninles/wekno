package agentruntime

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestPostgresMultimodalDefaultsOnceAndTenantModels(t *testing.T) {
	db := testsupport.Postgres(t, &types.CustomAgent{})
	require.NoError(t, db.Exec(`CREATE TABLE models (id text, tenant_id bigint, type text, is_default boolean, created_at timestamp, deleted_at timestamp)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO models VALUES
		('vlm',7,'VLLM',true,NOW(),NULL), ('asr',7,'ASR',true,NOW(),NULL),
		('other-tenant-vlm',8,'VLLM',true,NOW(),NULL)`).Error)
	for _, profile := range []string{types.AgentTypeKnowledgeQA, types.AgentTypeWikiQA,
		types.AgentTypeHybridRAGWiki, types.AgentTypeDataAnalysis, types.AgentTypeTableAnalysis,
		types.AgentTypeDocumentProcessingAgent, types.AgentTypeGeneralAgent,
		types.AgentTypeKnowledgeBaseManager, types.AgentTypeCustom} {
		require.NoError(t, db.Create(&types.CustomAgent{ID: profile, Name: profile, TenantID: 7,
			Config: types.CustomAgentConfig{AgentType: profile, KnowledgeBases: []string{"kb"}, VLMModelID: "chosen-vlm"}}).Error)
	}
	require.NoError(t, db.Create(&types.CustomAgent{ID: "empty", Name: "empty", TenantID: 9}).Error)
	require.NoError(t, migrateAgentProfiles(context.Background(), db))
	require.NoError(t, enableAgentMultimodalDefaults(context.Background(), db))
	var rows []types.CustomAgent
	require.NoError(t, db.Find(&rows).Error)
	for _, row := range rows {
		require.True(t, row.Config.ImageUploadEnabled)
		require.True(t, row.Config.AudioUploadEnabled)
		if row.TenantID == 7 {
			require.Equal(t, "chosen-vlm", row.Config.VLMModelID)
			require.Equal(t, "asr", row.Config.ASRModelID)
			require.Equal(t, []string{"kb"}, row.Config.KnowledgeBases)
		} else {
			require.Empty(t, row.Config.VLMModelID)
			require.Empty(t, row.Config.ASRModelID)
		}
	}
	// Explicit changes after the cutover survive a subsequent application restart.
	require.NoError(t, db.Exec(`UPDATE custom_agents SET config = config::jsonb || '{"image_upload_enabled":false,"audio_upload_enabled":false}'::jsonb`).Error)
	require.NoError(t, enableAgentMultimodalDefaults(context.Background(), db))
	var enabled int64
	require.NoError(t, db.Table("custom_agents").Where("config->>'image_upload_enabled' = 'true' OR config->>'audio_upload_enabled' = 'true'").Count(&enabled).Error)
	require.Zero(t, enabled)
}

package agentruntime

import (
	"context"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestPostgresRetireSimpleChatPreservesHistoryAndOtherAgents(t *testing.T) {
	db := testsupport.Postgres(t, &types.CustomAgent{})
	require.NoError(t, db.Exec(`CREATE TABLE custom_agent_profile_cutovers(version integer PRIMARY KEY);
CREATE TABLE sessions(id text PRIMARY KEY, agent_id text, agent_config jsonb);
CREATE TABLE messages(id text PRIMARY KEY, content text);
INSERT INTO sessions VALUES ('old', 'builtin-simple-chat', '{"agent_id":"builtin-simple-chat","other":"kept"}'), ('other','custom-1','{}');
INSERT INTO messages VALUES ('history','Original answer');`).Error)
	for _, tenant := range []uint64{7, 8} {
		require.NoError(t, db.Create(&types.CustomAgent{ID: "builtin-simple-chat", Name: "old", TenantID: tenant, IsBuiltin: true}).Error)
		require.NoError(t, db.Create(&types.CustomAgent{ID: "custom-1", Name: "kept", TenantID: tenant}).Error)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- retireSimpleChat(context.Background(), db) }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.NoError(t, retireSimpleChat(context.Background(), db))
	var count int64
	require.NoError(t, db.Unscoped().Model(&types.CustomAgent{}).Where("id = ?", "builtin-simple-chat").Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Model(&types.CustomAgent{}).Where("id = ?", "custom-1").Count(&count).Error)
	require.EqualValues(t, 2, count)
	var value string
	require.NoError(t, db.Raw(`SELECT agent_id FROM sessions WHERE id='old'`).Scan(&value).Error)
	require.Equal(t, types.BuiltinGeneralAgentID, value)
	require.NoError(t, db.Raw(`SELECT agent_config->>'agent_id' FROM sessions WHERE id='old'`).Scan(&value).Error)
	require.Equal(t, types.BuiltinGeneralAgentID, value)
	require.NoError(t, db.Raw(`SELECT agent_config->>'other' FROM sessions WHERE id='old'`).Scan(&value).Error)
	require.Equal(t, "kept", value)
	require.NoError(t, db.Raw(`SELECT agent_id FROM sessions WHERE id='other'`).Scan(&value).Error)
	require.Equal(t, "custom-1", value)
	require.NoError(t, db.Raw(`SELECT content FROM messages WHERE id='history'`).Scan(&value).Error)
	require.Equal(t, "Original answer", value)
}

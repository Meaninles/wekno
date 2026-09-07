package conversationmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"strings"
	"testing"
)

func TestArchivedUncitedEvidenceRemainsReadableAndPermissionChecked(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	conn, _ := db.DB()
	conn.SetMaxOpenConns(1)
	defer conn.Close()
	for _, sql := range []string{
		`CREATE TABLE sessions(id TEXT,tenant_id INTEGER,user_id TEXT,deleted_at DATETIME)`,
		`CREATE TABLE messages(id TEXT,session_id TEXT,role TEXT,content TEXT,is_completed BOOLEAN,created_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE custom_agent_runs(message_id TEXT,session_id TEXT,tenant_id INTEGER,status TEXT,"references" TEXT)`,
		`CREATE TABLE chunks(id TEXT,tenant_id INTEGER,knowledge_base_id TEXT,knowledge_id TEXT,is_enabled BOOLEAN,content TEXT,chunk_type TEXT,processing_generation TEXT,updated_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE knowledges(id TEXT,title TEXT,tenant_id INTEGER,knowledge_base_id TEXT,enable_status TEXT,core_status TEXT,published_generation TEXT,deleted_at DATETIME)`,
		`INSERT INTO sessions VALUES('s',7,'owner',NULL)`,
		`INSERT INTO messages VALUES('a','s','assistant','uncited summary',true,CURRENT_TIMESTAMP,NULL)`,
		`INSERT INTO chunks VALUES('c',7,'kb','d',true,'企业发展部负责高质量发展绩效考核','text','g1',NULL,NULL)`,
		`INSERT INTO knowledges VALUES('d','高质量发展绩效考核管理规定',7,'kb','enabled','ready','g1',NULL)`,
	} {
		require.NoError(t, db.Exec(sql).Error)
	}
	ref := &types.SearchResult{ID: "c", KnowledgeID: "d", KnowledgeBaseID: "kb", KnowledgeTitle: "高质量发展绩效考核管理规定", ChunkType: "text", Content: "企业发展部负责高质量发展绩效考核", EvidenceContent: "企业发展部负责高质量发展绩效考核"}
	sourcerefs.AssignCitationIDs([]*types.SearchResult{ref})
	body, err := json.Marshal([]*types.SearchResult{ref})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`INSERT INTO custom_agent_runs VALUES('a','s',7,'completed',?)`, string(body)).Error)
	ctx := context.WithValue(context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7)), types.UserIDContextKey, "owner")
	targets := types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TenantID: 7}}
	tool := &ReadTool{DB: db, SessionID: "s", SearchTargets: targets}
	for _, args := range []string{`{"section":"evidence","source_id":"assistant_message_a"}`, `{"section":"evidence","query":"他们在高质量发展上有哪些职责"}`} {
		result, err := tool.Execute(ctx, json.RawMessage(args))
		require.NoError(t, err)
		require.Len(t, result.SourceReferences, 1)
		require.Contains(t, result.Output, "企业发展部")
	}
	original := &types.Message{ID: "a", Role: "assistant", Content: "uncited summary"}
	projected, err := WithArchivedEvidence(ctx, db, "s", []*types.Message{original}, targets)
	require.NoError(t, err)
	require.Empty(t, original.KnowledgeReferences)
	require.Contains(t, string(AssistantMetadata(projected[0])), "高质量发展绩效考核管理规定")
	for _, change := range []string{`UPDATE chunks SET content='changed'`, `UPDATE knowledges SET published_generation='g2'`, `UPDATE knowledges SET enable_status='disabled'`} {
		require.NoError(t, db.Exec(change).Error)
		result, err := tool.Execute(ctx, json.RawMessage(`{"section":"evidence","source_id":"assistant_message_a"}`))
		require.NoError(t, err)
		require.Empty(t, result.SourceReferences)
		require.NoError(t, db.Exec(`UPDATE chunks SET content='企业发展部负责高质量发展绩效考核'`).Error)
		require.NoError(t, db.Exec(`UPDATE knowledges SET published_generation='g1',enable_status='enabled'`).Error)
	}
	tool.SearchTargets = nil
	result, err := tool.Execute(ctx, json.RawMessage(`{"section":"evidence","source_id":"assistant_message_a"}`))
	require.NoError(t, err)
	require.Empty(t, result.SourceReferences)
	_, err = tool.Execute(context.WithValue(ctx, types.UserIDContextKey, "other"), json.RawMessage(`{"section":"evidence","query":"高质量发展"}`))
	require.Error(t, err)
	archive, err := LoadEvidenceArchive(ctx, db, "another-session", []string{"a"})
	require.NoError(t, err)
	require.Empty(t, archive)
	require.NoError(t, db.Exec(`ALTER TABLE messages ADD COLUMN error_code TEXT DEFAULT ''`).Error)
	require.NoError(t, db.Exec(`UPDATE messages SET error_code='unknown',content='这次未能完成，请稍后重试。' WHERE id='a'`).Error)
	result, err = tool.Execute(ctx, json.RawMessage(`{"section":"text","source_id":"assistant_message_a"}`))
	require.NoError(t, err)
	require.Contains(t, result.Output, `"outcome":"failed"`)
	require.Contains(t, result.Output, `"text":""`)
	require.NotContains(t, result.Output, "这次未能完成")
	wrongTenant := context.WithValue(ctx, types.TenantIDContextKey, uint64(8))
	archive, err = LoadEvidenceArchive(wrongTenant, db, "s", []string{"a"})
	require.NoError(t, err)
	require.Empty(t, archive)
}

func TestEvidenceSelectionBoundsContextAndFindsUncitedNewTopic(t *testing.T) {
	refs := []*types.SearchResult{}
	for i := 0; i < 228; i++ {
		refs = append(refs, &types.SearchResult{ID: fmt.Sprint(i), KnowledgeID: "master", KnowledgeBaseID: "kb", ChunkType: "text", KnowledgeTitle: "主数据管理规定", EvidenceContent: strings.Repeat("主数据编码表格", 100)})
	}
	target := &types.SearchResult{ID: "target", KnowledgeID: "performance", KnowledgeBaseID: "kb", ChunkType: "text", KnowledgeTitle: "高质量发展绩效考核管理规定", EvidenceContent: "企业发展部负责高质量发展绩效考核，制定指标及年度方案。"}
	refs = append(refs, target, target)
	got := SelectEvidence(refs, "他们在高质量发展上有哪些职责", 4000)
	require.Len(t, got, 1)
	require.Equal(t, "target", got[0].ID)
	require.Empty(t, SelectEvidence(refs, "你好", 4000))
	require.Empty(t, SelectEvidence(refs, "高质量发展", 1))
}

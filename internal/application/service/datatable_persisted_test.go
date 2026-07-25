package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type persistedTableChatStub struct {
	prompts []string
}

func (s *persistedTableChatStub) Chat(
	_ context.Context,
	messages []chat.Message,
	_ *chat.ChatOptions,
) (*types.ChatResponse, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("missing prompt")
	}
	s.prompts = append(s.prompts, messages[len(messages)-1].Content)
	return &types.ChatResponse{Content: "generated metadata"}, nil
}

func (*persistedTableChatStub) ChatStream(
	context.Context,
	[]chat.Message,
	*chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*persistedTableChatStub) GetModelName() string { return "persisted-table-stub" }
func (*persistedTableChatStub) GetModelID() string   { return "persisted-table-stub" }

type persistedTableEnqueuerStub struct {
	interfaces.TaskEnqueuer
}

func TestProcessPersistedTableDataSupportsLegacyXLSWithoutDuckDB(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}, &documentsplit.Plan{}))

	const generation = "generation-xls"
	require.NoError(t, db.Create(&types.Chunk{
		ID:                   "00000000-0000-0000-0000-000000000731",
		TenantID:             7,
		KnowledgeID:          "knowledge-xls",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: generation,
		ChunkIndex:           0,
		ChunkType:            types.ChunkTypeText,
		Content:              "标识: WKN-FORMAT-XLS-7319,责任部门: 数字化管理部门,完成时限: 三个工作日,控制要求: 未经授权不得跳过安全检查",
	}).Error)

	manager := documentsplit.NewManagerWithConfig(
		db, &persistedTableEnqueuerStub{}, documentsplit.Config{},
	)
	model := &persistedTableChatStub{}
	service := &DataTableSummaryService{splitManager: manager}
	resources := &extractionResources{
		knowledge: &types.Knowledge{
			ID:                   "knowledge-xls",
			TenantID:             7,
			KnowledgeBaseID:      "kb-1",
			FileName:             "control.xls",
			FileType:             "xls",
			FileSize:             4_096,
			ProcessingGeneration: generation,
		},
		chatModel: model,
	}

	chunks, err := service.processTableData(context.Background(), resources)
	require.NoError(t, err)
	require.Len(t, chunks, 2)
	require.Equal(t, types.ChunkTypeTableSummary, chunks[0].ChunkType)
	require.Equal(t, types.ChunkTypeTableColumn, chunks[1].ChunkType)
	require.Len(t, model.prompts, 2)
	require.Contains(t, model.prompts[0], "WKN-FORMAT-XLS-7319")
	require.Contains(t, model.prompts[0], "责任部门")
}

func TestProcessPersistedTableDataRejectsOtherGenerationChunks(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}, &documentsplit.Plan{}))
	require.NoError(t, db.Create(&types.Chunk{
		ID:                   "00000000-0000-0000-0000-000000000732",
		TenantID:             7,
		KnowledgeID:          "knowledge-xls",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "stale-generation",
		ChunkIndex:           0,
		ChunkType:            types.ChunkTypeText,
		Content:              "stale data must not leak",
	}).Error)

	manager := documentsplit.NewManagerWithConfig(
		db, &persistedTableEnqueuerStub{}, documentsplit.Config{},
	)
	service := &DataTableSummaryService{splitManager: manager}
	_, err = service.processTableData(context.Background(), &extractionResources{
		knowledge: &types.Knowledge{
			ID:                   "knowledge-xls",
			TenantID:             7,
			KnowledgeBaseID:      "kb-1",
			FileName:             "control.xls",
			FileType:             "xls",
			ProcessingGeneration: "current-generation",
		},
		chatModel: &persistedTableChatStub{},
	})
	require.ErrorContains(t, err, "current generation has no text chunks")
}

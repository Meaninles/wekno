package chatpipeline

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingEventBus struct {
	events []types.Event
}

func (b *recordingEventBus) On(types.EventType, types.EventHandler) {}

func (b *recordingEventBus) Emit(_ context.Context, evt types.Event) error {
	b.events = append(b.events, evt)
	return nil
}

func TestIsConsolidatedRetrievalStage(t *testing.T) {
	cm := &types.ChatManage{}
	assert.True(t, IsConsolidatedRetrievalStage(types.CHUNK_SEARCH_PARALLEL, cm))
	assert.False(t, IsConsolidatedRetrievalStage(types.QUERY_UNDERSTAND, cm))
	assert.False(t, IsConsolidatedRetrievalStage(types.LOAD_HISTORY, cm))
}

func TestLastConsolidatedRetrievalStage(t *testing.T) {
	cm := &types.ChatManage{}
	pipeline := []types.EventType{
		types.LOAD_HISTORY,
		types.QUERY_UNDERSTAND,
		types.CHUNK_SEARCH_PARALLEL,
		types.CHUNK_RERANK,
		types.CHUNK_MERGE,
		types.FILTER_TOP_K,
		types.INTO_CHAT_MESSAGE,
		types.CHAT_COMPLETION_STREAM,
	}
	assert.Equal(t, types.FILTER_TOP_K, LastConsolidatedRetrievalStage(pipeline, cm))
}

func TestShouldEndRetrievalProgress(t *testing.T) {
	tests := []struct {
		name    string
		current types.EventType
		last    types.EventType
		err     *PluginError
		want    bool
	}{
		{
			name:    "normal final stage",
			current: types.FILTER_TOP_K,
			last:    types.FILTER_TOP_K,
			want:    true,
		},
		{
			name:    "early no results",
			current: types.CHUNK_SEARCH_PARALLEL,
			last:    types.FILTER_TOP_K,
			err:     ErrSearchNothing,
			want:    true,
		},
		{
			name:    "early search failure",
			current: types.CHUNK_SEARCH_PARALLEL,
			last:    types.FILTER_TOP_K,
			err:     ErrSearch,
			want:    true,
		},
		{
			name:    "healthy intermediate stage",
			current: types.CHUNK_RERANK,
			last:    types.FILTER_TOP_K,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ShouldEndRetrievalProgress(tt.current, tt.last, tt.err))
		})
	}
}

func TestShouldEmitQueryUnderstandProgress(t *testing.T) {
	cm := &types.ChatManage{PipelineRequest: types.PipelineRequest{EnableRewrite: true}}
	assert.True(t, ShouldEmitQueryUnderstandProgress(cm))

	cm.EnableRewrite = false
	assert.False(t, ShouldEmitQueryUnderstandProgress(cm))

	cm.Images = []string{"data:image/png;base64,abc"}
	assert.True(t, ShouldEmitQueryUnderstandProgress(cm))
}

func TestQueryUnderstandProgressEmitsToolCallAndResult(t *testing.T) {
	bus := &recordingEventBus{}
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SessionID: "sess-1", EnableRewrite: true},
		PipelineContext: types.PipelineContext{EventBus: bus},
	}

	start := time.Now()
	progress := BeginQueryUnderstandProgress(context.Background(), cm)
	require.NotNil(t, progress)
	EndQueryUnderstandProgress(context.Background(), cm, progress, start, nil)

	require.Len(t, bus.events, 2)
	callData, ok := bus.events[0].Data.(event.AgentToolCallData)
	require.True(t, ok)
	assert.Equal(t, "query_understand", callData.ToolName)
}

func TestRetrievalProgressEmitsSingleToolCallAndResult(t *testing.T) {
	bus := &recordingEventBus{}
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SessionID: "sess-1"},
		PipelineContext: types.PipelineContext{EventBus: bus},
		PipelineState: types.PipelineState{
			MergeResult: []*types.SearchResult{{ID: "r1"}, {ID: "r2"}, {ID: "r3"}},
		},
	}

	start := time.Now()
	progress := BeginRetrievalProgress(context.Background(), cm)
	require.NotNil(t, progress)
	EndRetrievalProgress(context.Background(), cm, progress, start, nil)

	require.Len(t, bus.events, 2)
	assert.Equal(t, types.EventType(event.EventAgentToolCall), bus.events[0].Type)
	assert.Equal(t, types.EventType(event.EventAgentToolResult), bus.events[1].Type)

	callData, ok := bus.events[0].Data.(event.AgentToolCallData)
	require.True(t, ok)
	assert.Equal(t, "knowledge_search", callData.ToolName)

	resultData, ok := bus.events[1].Data.(event.AgentToolResultData)
	require.True(t, ok)
	assert.True(t, resultData.Success)
	assert.Equal(t, 3, resultData.Data["count"])
}

func TestRetrievalProgressRecordsEarlyNoResults(t *testing.T) {
	bus := &recordingEventBus{}
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SessionID: "sess-1", Query: "没有命中的问题"},
		PipelineContext: types.PipelineContext{EventBus: bus},
		PipelineState: types.PipelineState{
			// Early retrieval candidates may exist even though reranking rejects
			// all of them. The user-visible terminal count must still be zero.
			SearchResult: []*types.SearchResult{{ID: "rejected-candidate"}},
		},
	}

	progress := BeginRetrievalProgress(context.Background(), cm)
	require.NotNil(t, progress)
	EndRetrievalProgress(context.Background(), cm, progress, time.Now(), ErrSearchNothing)

	require.Len(t, bus.events, 2)
	resultData, ok := bus.events[1].Data.(event.AgentToolResultData)
	require.True(t, ok)
	assert.True(t, resultData.Success)
	assert.Equal(t, 0, resultData.Data["count"])
	assert.Equal(t, "检索完成", resultData.Output)
}

func TestRetrievalProgressUsesWebSearchForWebOnlyRequest(t *testing.T) {
	bus := &recordingEventBus{}
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SessionID: "sess-1", WebSearchEnabled: true},
		PipelineContext: types.PipelineContext{EventBus: bus},
		PipelineState: types.PipelineState{
			SearchResult: []*types.SearchResult{
				{ID: "https://example.com/1", KnowledgeSource: "web_search"},
				{ID: "https://example.com/2", KnowledgeSource: "web_search"},
			},
		},
	}

	start := time.Now()
	progress := BeginRetrievalProgress(context.Background(), cm)
	require.NotNil(t, progress)
	EndRetrievalProgress(context.Background(), cm, progress, start, nil)

	require.Len(t, bus.events, 2)
	callData, ok := bus.events[0].Data.(event.AgentToolCallData)
	require.True(t, ok)
	assert.Equal(t, "web_search", callData.ToolName)

	resultData, ok := bus.events[1].Data.(event.AgentToolResultData)
	require.True(t, ok)
	assert.Equal(t, "web_search", resultData.ToolName)
	assert.Equal(t, 2, resultData.Data["count"])
	assert.Contains(t, resultData.Output, "网络搜索结果")
}

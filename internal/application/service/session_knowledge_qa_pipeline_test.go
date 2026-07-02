package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

func TestBuildKnowledgeQAPipelineSkipsRerankForWebOnly(t *testing.T) {
	pipeline := buildKnowledgeQAPipeline(true, false, true, false, false)

	assert.Equal(t, []types.EventType{
		types.LOAD_HISTORY,
		types.QUERY_UNDERSTAND,
		types.CHUNK_SEARCH_PARALLEL,
		types.CHUNK_MERGE,
		types.FILTER_TOP_K,
		types.INTO_CHAT_MESSAGE,
		types.CHAT_COMPLETION_STREAM,
	}, pipeline)
}

func TestBuildKnowledgeQAPipelineKeepsRerankForKnowledgeSearch(t *testing.T) {
	pipeline := buildKnowledgeQAPipeline(false, true, true, true, true)

	assert.Equal(t, []types.EventType{
		types.QUERY_UNDERSTAND,
		types.CHUNK_SEARCH_PARALLEL,
		types.CHUNK_RERANK,
		types.WEB_FETCH,
		types.CHUNK_MERGE,
		types.FILTER_TOP_K,
		types.DATA_ANALYSIS,
		types.INTO_CHAT_MESSAGE,
		types.CHAT_COMPLETION_STREAM,
	}, pipeline)
}

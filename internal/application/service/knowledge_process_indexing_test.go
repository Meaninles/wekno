package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

func TestIndexableTextChunksIncludesUnparentedParentChildTail(t *testing.T) {
	parent := &types.Chunk{
		ID:        "parent",
		ChunkType: types.ChunkTypeParentText,
	}
	child := &types.Chunk{
		ID:            "child",
		ChunkType:     types.ChunkTypeText,
		ParentChunkID: parent.ID,
	}
	tail := &types.Chunk{
		ID:        "unparented-tail",
		ChunkType: types.ChunkTypeText,
	}
	summary := &types.Chunk{
		ID:        "summary",
		ChunkType: types.ChunkTypeSummary,
	}

	got := indexableTextChunks([]*types.Chunk{
		parent, child, nil, tail, summary,
	})

	assert.Equal(t, []*types.Chunk{child, tail}, got)
}

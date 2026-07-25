package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/contentcache"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestWikiMapContentCacheKeyIsScopedToDocumentGeneration(t *testing.T) {
	contentHash := contentcache.Digest("identical transcribed content")
	key := func(knowledgeID, generation string) contentcache.Key {
		return wikiMapContentCacheKey(
			42,
			knowledgeID,
			generation,
			contentHash,
			nil,
			"zh",
			types.WikiExtractionStandard,
			nil,
		)
	}

	first := key("knowledge-a", "generation-a")
	require.Equal(t, first, key("knowledge-a", "generation-a"))
	require.NotEqual(t, first.ContentHash, key("knowledge-b", "generation-a").ContentHash)
	require.NotEqual(t, first.ContentHash, key("knowledge-a", "generation-b").ContentHash)
}

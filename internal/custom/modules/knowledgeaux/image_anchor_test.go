package knowledgeaux

import (
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestRepairExtractedImageDoesNotAcquireInventedParagraph(t *testing.T) {
	original := types.ParsedChunk{Content: "A paragraph about another subject.", Seq: 7, Start: 10, End: 44, ParentIndex: 3}
	chunks := EnsureImageAnchors([]types.ParsedChunk{original}, []string{"minio://picture", "minio://picture"})
	require.Len(t, chunks, 2)
	require.Equal(t, original, chunks[0])
	require.Equal(t, -1, chunks[1].ParentIndex)
	require.Equal(t, 8, chunks[1].Seq)
	var locator map[string]any
	require.NoError(t, json.Unmarshal(chunks[1].SourceLocator, &locator))
	require.Equal(t, false, locator["position_known"])
	require.Equal(t, "minio://picture", locator["image_url"])
	require.Len(t, EnsureImageAnchors(chunks, []string{"minio://picture"}), 2)
}

package chatretrieval

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestImageObservationsAreRetainedOutsideExactDocumentBody(t *testing.T) {
	images, err := json.Marshal([]types.ImageInfo{{URL: "stored.png", OriginalURL: "original.png", OCRText: "Recognized label", Caption: "Generated interpretation"}})
	require.NoError(t, err)
	ref := &types.SearchResult{ID: "chunk", Content: "Original paragraph", EvidenceContent: "Original paragraph", ImageInfo: string(images), ChunkType: "text"}
	output := EvidenceText(ref)
	body, observations, found := strings.Cut(output, "[/EXACT_FRAGMENT]")
	require.True(t, found)
	require.Contains(t, body, "Original paragraph")
	require.NotContains(t, body, "Recognized label")
	require.NotContains(t, body, "Generated interpretation")
	require.Contains(t, observations, `"derived":true`)
	require.Contains(t, observations, `"original_image_url":"original.png"`)
	require.Contains(t, observations, `"ocr_text":"Recognized label"`)
	require.Contains(t, observations, `"description":"Generated interpretation"`)
	require.Equal(t, 1, strings.Count(output, "Recognized label"))
	require.Contains(t, Passage(ref), "Recognized label") // It remains useful for retrieval ranking.
	require.Equal(t, string(images), ref.ImageInfo)
}

func TestDirectImageChunkDoesNotClaimToBeExactSourceText(t *testing.T) {
	for _, kind := range []types.ChunkType{types.ChunkTypeImageOCR, types.ChunkTypeImageCaption} {
		output := EvidenceText(&types.SearchResult{ID: "image", Content: "Machine reading", ChunkType: string(kind)})
		require.NotContains(t, output, "EXACT_FRAGMENT")
		require.Contains(t, output, "IMAGE_OBSERVATIONS")
		require.Contains(t, output, "Machine reading")
	}
}

package tools

import (
	"context"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

type evidenceChunkService struct{ interfaces.ChunkService }

func (evidenceChunkService) GetRepository() interfaces.ChunkRepository { return nil }

// Exercise the real tool boundary and registry, including a repeated call after
// old tool payloads may have been archived. A per-tool "seen" map must not hide
// requested evidence from the current context.
func TestKnowledgeSearchEvidenceRemainsReadableAndCitable(t *testing.T) {
	tool := &KnowledgeSearchTool{chunkService: evidenceChunkService{}}
	input := []*searchResultWithMeta{{SearchResult: &types.SearchResult{ID: "chunk-a", KnowledgeID: "doc-a", KnowledgeBaseID: "kb-a", ChunkType: string(types.ChunkTypeText), Content: "physical source text", ImageInfo: `[{"ocr_text":"physical source text","caption":"image description"}]`}}}
	for range 2 {
		result, err := tool.formatOutput(context.Background(), input, []string{"kb-a"}, []string{"query"}, map[string]string{"kb-a": "collection"})
		require.NoError(t, err)
		require.Len(t, result.SourceReferences, 1)
		require.Equal(t, 1, strings.Count(result.Output, "physical source text"))
		require.Contains(t, result.Output, "image description")
		registry := sourcerefs.NewRegistry()
		refs, _ := sourcerefs.RegisterToolResult(registry, "knowledge_search", result)
		output := sourcerefs.AppendCitationCatalog(result.Output, refs)
		require.Contains(t, output, "citation_handle_for_this_evidence:")
		require.NotContains(t, output, "already_seen")
	}
}

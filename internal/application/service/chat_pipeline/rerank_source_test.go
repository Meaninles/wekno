package chatpipeline

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestRerankPassagePreservesSourceStructure(t *testing.T) {
	for _, content := range []string{
		"| Object | Limit |\n| --- | --- |\n| Type A | 300 |\n| Type B | 500 |",
		"```sql\nSELECT amount FROM invoices WHERE approved = true\n```",
		"Formula: $$E=mc^2$$. See [method](https://example.org/method).",
	} {
		result := &types.SearchResult{Content: content}
		require.Equal(t, content, getEnrichedPassage(context.Background(), result))
	}
}

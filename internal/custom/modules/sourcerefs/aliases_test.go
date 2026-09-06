package sourcerefs

import (
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGroupedCitationAliasesRequireEveryHandleToBeRegistered(t *testing.T) {
	refs := []*types.SearchResult{
		{ID: "a", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: "text", Content: "source A"},
		{ID: "b", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: "text", Content: "source B"},
	}
	AssignCitationIDs(refs)
	for _, group := range []string{"（S1、S2）", "[S1, S2]", "(S1;S2)"} {
		require.Equal(t, `claim <src id="S1" /><src id="S2" />`, normalizeKnownCitationAliases("claim "+group, refs))
	}
	for _, input := range []string{"（S1、S99）", "`(S1, S2)`", "(S1 + S2)", "source S1 is unrelated prose"} {
		require.Equal(t, input, normalizeKnownCitationAliases(input, refs))
	}
}

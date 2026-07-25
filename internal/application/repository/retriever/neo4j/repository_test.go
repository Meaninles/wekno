package neo4j

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestLabelsIncludeStableIdentityLabel(t *testing.T) {
	repository := &Neo4jRepository{nodePrefix: "ENTITY"}
	namespace := types.NameSpace{
		KnowledgeBase: "kb-with-hyphen",
		Knowledge:     "doc-with-hyphen",
	}

	require.Equal(t, []string{
		graphEntityBaseLabel,
		"ENTITYkb_with_hyphen",
		"ENTITYdoc_with_hyphen",
	}, repository.Labels(namespace))
	require.Equal(
		t,
		"ENTITYkb_with_hyphen:ENTITYdoc_with_hyphen",
		repository.Label(namespace),
	)
}

func TestLabelsAlwaysRetainBaseLabel(t *testing.T) {
	repository := &Neo4jRepository{nodePrefix: "ENTITY"}
	require.Equal(t, []string{graphEntityBaseLabel}, repository.Labels(types.NameSpace{}))
	require.Empty(t, repository.Label(types.NameSpace{}))
}

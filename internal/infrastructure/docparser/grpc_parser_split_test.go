package docparser

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetMaxSplitExpansionRatioIsBounded(t *testing.T) {
	t.Setenv("CUSTOM_DOCUMENT_SPLIT_MAX_EXPANSION_RATIO", "24.5")
	require.Equal(t, 24.5, getMaxSplitExpansionRatio())

	for _, invalid := range []string{"", "0", "101", "not-a-number"} {
		t.Run(invalid, func(t *testing.T) {
			t.Setenv("CUSTOM_DOCUMENT_SPLIT_MAX_EXPANSION_RATIO", invalid)
			require.Equal(t, 12.0, getMaxSplitExpansionRatio())
		})
	}
}

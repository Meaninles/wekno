package wikiqueue

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIngestDedupIdentity(t *testing.T) {
	key, err := IngestDedupKey("knowledge-1", "generation-1")
	require.NoError(t, err)
	require.Equal(t, "knowledge-1:generation-1", key)

	prefix, err := IngestDedupPrefix("knowledge-1")
	require.NoError(t, err)
	require.Equal(t, "knowledge-1:", prefix)
	require.Error(t, func() error {
		_, err := IngestDedupKey("knowledge-1", "")
		return err
	}())
	require.Error(t, func() error {
		_, err := IngestDedupPrefix("knowledge:1")
		return err
	}())
}

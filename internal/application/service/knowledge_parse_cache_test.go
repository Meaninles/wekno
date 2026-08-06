package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/contentcache"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestDocumentParseCacheIdentityIncludesSemanticParserVersion(t *testing.T) {
	payload := types.DocumentProcessPayload{FileType: "XLSX"}
	knowledge := &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             10000,
		FileHash:             "source-hash",
		FileSize:             9425,
		ProcessingGeneration: "generation-1",
	}
	overrides := map[string]string{"xlsx": "builtin"}

	key, ref, eligible := documentParseCacheIdentity(
		payload,
		knowledge,
		"builtin",
		overrides,
	)

	require.True(t, eligible)
	require.Equal(t, contentcache.KindParse, key.Kind)
	require.Equal(t, knowledge.ID, ref.KnowledgeID)
	require.Equal(t, knowledge.ProcessingGeneration, ref.ProcessingGeneration)
	require.Equal(t, contentcache.Digest(
		documentParseCacheVersion,
		"builtin",
		"xlsx",
		`{"xlsx":"builtin"}`,
	), key.VersionHash)
	require.NotEqual(t, contentcache.Digest(
		"document-parser-v3-vector-images",
		"builtin",
		"xlsx",
		`{"xlsx":"builtin"}`,
	), key.VersionHash)
}

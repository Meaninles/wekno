package wikidelete

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuarantineClearContextCarriesExactDetachedSources(t *testing.T) {
	ctx := WithQuarantineClear(context.Background(), " knowledge-b ", "knowledge-a", "knowledge-a", "")
	first := ClearSources(ctx)
	assert.Equal(t, []string{"knowledge-a", "knowledge-b"}, first)
	first[0] = "mutated-by-caller"
	assert.Equal(t, []string{"knowledge-a", "knowledge-b"}, ClearSources(ctx))
	assert.Empty(t, ClearSources(WithQuarantineClear(context.Background())))
}

func TestQuarantineCompletePreservesMetadataAndRestoresVisibility(t *testing.T) {
	page := &types.WikiPage{
		Status:       types.WikiPageStatusDraft,
		PageMetadata: types.JSON(`{"owner":"user"}`),
	}
	require.NoError(t, Quarantine(page, "knowledge-b", "knowledge-a"))
	require.NoError(t, Quarantine(page, "knowledge-a"))
	assert.Equal(t, types.WikiPageStatusArchived, page.Status)
	assert.Equal(t, []string{"knowledge-a", "knowledge-b"}, mustPendingSources(t, page))

	require.NoError(t, MarkApplied(page, 11, 11, 12))
	require.NoError(t, Complete(page, "knowledge-a"))
	assert.Equal(t, types.WikiPageStatusArchived, page.Status)
	assert.Equal(t, []string{"knowledge-b"}, mustPendingSources(t, page))

	require.NoError(t, Complete(page, "knowledge-b"))
	assert.Equal(t, types.WikiPageStatusDraft, page.Status)
	assert.Empty(t, mustPendingSources(t, page))
	applied, err := IsApplied(page, 11)
	require.NoError(t, err)
	assert.True(t, applied)
	applied, err = IsApplied(page, 12)
	require.NoError(t, err)
	assert.True(t, applied)

	var metadata map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(page.PageMetadata, &metadata))
	assert.JSONEq(t, `"user"`, string(metadata["owner"]))
}

func TestPreserveBlocksStaleUnquarantine(t *testing.T) {
	current := &types.WikiPage{
		Status:       types.WikiPageStatusPublished,
		PageMetadata: types.JSON(`{"server":"current"}`),
	}
	require.NoError(t, Quarantine(current, "knowledge-1"))

	incoming := &types.WikiPage{
		Status:       types.WikiPageStatusPublished,
		PageMetadata: types.JSON(`{"writer":"stale"}`),
	}
	require.NoError(t, Preserve(current, incoming))
	assert.Equal(t, types.WikiPageStatusArchived, incoming.Status)
	assert.Equal(t, []string{"knowledge-1"}, mustPendingSources(t, incoming))

	var metadata map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(incoming.PageMetadata, &metadata))
	assert.JSONEq(t, `"stale"`, string(metadata["writer"]))
}

func TestAppliedMarkerIsBoundedAndRejectsCorruptMetadata(t *testing.T) {
	page := &types.WikiPage{PageMetadata: types.JSON(`{"_weknora_applied_retract_ops":"bad"}`)}
	_, err := IsApplied(page, 1)
	require.Error(t, err)

	page.PageMetadata = types.JSON(`{}`)
	ids := make([]int64, 0, maxAppliedOpsPerPage+10)
	for id := int64(1); id <= maxAppliedOpsPerPage+10; id++ {
		ids = append(ids, id)
	}
	require.NoError(t, MarkApplied(page, ids...))
	oldest, err := IsApplied(page, 1)
	require.NoError(t, err)
	assert.False(t, oldest)
	newest, err := IsApplied(page, int64(maxAppliedOpsPerPage+10))
	require.NoError(t, err)
	assert.True(t, newest)
}

func mustPendingSources(t *testing.T, page *types.WikiPage) []string {
	t.Helper()
	sources, err := PendingSources(page)
	require.NoError(t, err)
	return sources
}

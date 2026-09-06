package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/stretchr/testify/require"
)

func TestPostgresChunkStatusRetryDoesNotRewriteUnchangedVectors(t *testing.T) {
	db := testsupport.Postgres(t)
	require.NoError(t, db.Exec(`CREATE TABLE embeddings (id BIGINT PRIMARY KEY,
		chunk_id TEXT, is_enabled BOOLEAN, updated_at TIMESTAMP)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO embeddings VALUES
		(1, 'a', false, '2020-01-01'), (2, 'b', true, '2020-01-01'),
		(3, 'c', NULL, '2020-01-01'), (4, 'unselected', false, '2020-01-01')`).Error)
	repo := &pgRepository{db: db}
	statuses := map[string]bool{"a": true, "b": false, "c": false}
	require.NoError(t, repo.BatchUpdateChunkEnabledStatus(context.Background(), statuses))
	type row struct {
		ID        int
		IsEnabled bool
		UpdatedAt time.Time
	}
	var first, retried []row
	require.NoError(t, db.Table("embeddings").Order("id").Find(&first).Error)
	require.Len(t, first, 4)
	require.True(t, first[0].IsEnabled)
	require.False(t, first[1].IsEnabled)
	require.False(t, first[2].IsEnabled)
	require.Equal(t, 2020, first[3].UpdatedAt.Year())
	require.NoError(t, repo.BatchUpdateChunkEnabledStatus(context.Background(), statuses))
	require.NoError(t, db.Table("embeddings").Order("id").Find(&retried).Error)
	require.Equal(t, first, retried, "retry must preserve both flags and timestamps")
}

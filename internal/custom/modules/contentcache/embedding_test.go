package contentcache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type countingEmbedder struct {
	mu    sync.Mutex
	calls int
}

func (e *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := e.BatchEmbed(ctx, []string{text})
	return out[0], err
}
func (e *countingEmbedder) BatchEmbed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	out := make([][]float32, len(texts))
	for index, text := range texts {
		out[index] = []float32{float32(len(text)), float32(index + 1)}
	}
	return out, nil
}
func (e *countingEmbedder) BatchEmbedWithPool(ctx context.Context, _ embedding.Embedder, texts []string) ([][]float32, error) {
	return e.BatchEmbed(ctx, texts)
}
func (*countingEmbedder) GetModelName() string { return "counting" }
func (*countingEmbedder) GetDimensions() int   { return 2 }
func (*countingEmbedder) GetModelID() string   { return "model-1" }

func TestCachedEmbedderBatchesMissesAndPreservesDuplicateOrder(t *testing.T) {
	store, _ := newTestStore(t)
	inner := &countingEmbedder{}
	wrapped := WrapEmbedder(store, "model-version", inner)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	first, err := wrapped.BatchEmbed(ctx, []string{"alpha", "beta", "alpha"})
	require.NoError(t, err)
	require.Equal(t, []float32{5, 1}, first[0])
	require.Equal(t, first[0], first[2])
	require.Equal(t, 1, inner.calls)

	second, err := wrapped.BatchEmbed(ctx, []string{"beta", "alpha"})
	require.NoError(t, err)
	require.Equal(t, first[1], second[0])
	require.Equal(t, first[0], second[1])
	require.Equal(t, 1, inner.calls, "second call must be fully served by the shared cache")
}

func TestCachedEmbedderEvictsInvalidVectorBeforeRecompute(t *testing.T) {
	store, db := newTestStore(t)
	inner := &countingEmbedder{}
	wrapped := WrapEmbedder(store, "model-version", inner)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	_, err := wrapped.BatchEmbed(ctx, []string{"alpha"})
	require.NoError(t, err)
	require.Equal(t, 1, inner.calls)

	key := Key{
		TenantID: 7, Kind: KindEmbedding,
		ContentHash: Digest("document", "alpha"),
		VersionHash: Digest("embedding-cache-v1", "model-version"),
	}
	invalidVector := encodeVector([]float32{1})
	encoded, err := encodePayload(invalidVector)
	require.NoError(t, err)
	require.NoError(t, db.Model(&entry{}).Where(
		"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
		key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
	).Updates(map[string]any{
		"payload": encoded, "payload_sha256": payloadDigest(invalidVector),
		"payload_size": len(invalidVector), "updated_at": time.Now(),
	}).Error)

	vector, err := wrapped.Embed(ctx, "alpha")
	require.NoError(t, err)
	require.Len(t, vector, 2)
	require.Equal(t, 2, inner.calls)
	_, err = wrapped.Embed(ctx, "alpha")
	require.NoError(t, err)
	require.Equal(t, 2, inner.calls, "recomputed vector must replace the invalid immutable entry")
}

func TestCachedEmbedderEvictsChecksumCorruptionBeforeRecompute(t *testing.T) {
	store, db := newTestStore(t)
	inner := &countingEmbedder{}
	wrapped := WrapEmbedder(store, "model-version", inner)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(8))
	_, err := wrapped.Embed(ctx, "alpha")
	require.NoError(t, err)
	require.NoError(t, db.Model(&entry{}).Where("tenant_id = ?", 8).
		UpdateColumn("payload_sha256", Digest("corrupt")).Error)

	_, err = wrapped.Embed(ctx, "alpha")
	require.NoError(t, err)
	require.Equal(t, 2, inner.calls)
	_, err = wrapped.Embed(ctx, "alpha")
	require.NoError(t, err)
	require.Equal(t, 2, inner.calls)
}

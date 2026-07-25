package contentcache

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
)

const embeddingCacheTTL = 30 * 24 * time.Hour

type cachedEmbedder struct {
	store       *Store
	versionHash string
	inner       embedding.Embedder
}

func WrapEmbedder(store *Store, version string, inner embedding.Embedder) embedding.Embedder {
	if store == nil || inner == nil {
		return inner
	}
	return &cachedEmbedder{
		store: store, versionHash: Digest("embedding-cache-v1", version), inner: inner,
	}
}

func (e *cachedEmbedder) GetModelName() string { return e.inner.GetModelName() }
func (e *cachedEmbedder) GetDimensions() int   { return e.inner.GetDimensions() }
func (e *cachedEmbedder) GetModelID() string   { return e.inner.GetModelID() }

func (e *cachedEmbedder) GetMaxInputTokens() int {
	return embedding.MaxInputTokens(e.inner)
}

func (e *cachedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.BatchEmbed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embedding cache: expected one vector, got %d", len(vectors))
	}
	return vectors[0], nil
}

func (e *cachedEmbedder) BatchEmbedWithPool(
	ctx context.Context,
	_ embedding.Embedder,
	texts []string,
) ([][]float32, error) {
	return e.batchEmbed(ctx, texts, true)
}

func (e *cachedEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.batchEmbed(ctx, texts, false)
}

func (e *cachedEmbedder) batchEmbed(
	ctx context.Context,
	texts []string,
	usePool bool,
) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return e.inner.BatchEmbed(ctx, texts)
	}
	mode := "document"
	if query, _ := ctx.Value(types.EmbedQueryContextKey).(bool); query {
		mode = "query"
	}

	keys := make([]Key, len(texts))
	uniqueKeys := make([]Key, 0, len(texts))
	seen := make(map[string]struct{}, len(texts))
	for index, text := range texts {
		key := Key{
			TenantID: tenantID, Kind: KindEmbedding,
			ContentHash: Digest(mode, text), VersionHash: e.versionHash,
		}
		keys[index] = key
		if _, exists := seen[key.ContentHash]; !exists {
			seen[key.ContentHash] = struct{}{}
			uniqueKeys = append(uniqueKeys, key)
		}
	}
	hits, err := e.store.GetMany(ctx, uniqueKeys, Reference{})
	if err != nil {
		// Cache availability and integrity must never become an ingestion
		// dependency. PostgreSQL may be healthy for core tables while this
		// optional table is temporarily locked or awaiting migration during a
		// rolling deployment; recompute and keep the document moving.
		logger.Warnf(ctx, "embedding cache lookup failed; recomputing batch: %v", err)
		if errors.Is(err, ErrCorruptPayload) {
			e.evictEmbeddingKeys(ctx, uniqueKeys)
		}
		hits = map[string][]byte{}
	}

	results := make([][]float32, len(texts))
	missingTexts := make([]string, 0)
	missingHashes := make([]string, 0)
	missingSeen := make(map[string]struct{})
	invalidKeys := make(map[string]Key)
	for index, key := range keys {
		if encoded, exists := hits[key.ContentHash]; exists {
			vector, decodeErr := decodeVector(encoded)
			if decodeErr == nil && vectorDimensionsValid(vector, e.inner.GetDimensions()) {
				results[index] = vector
				continue
			}
			delete(hits, key.ContentHash)
			invalidKeys[key.ContentHash] = key
		}
		if _, exists := missingSeen[key.ContentHash]; exists {
			continue
		}
		missingSeen[key.ContentHash] = struct{}{}
		missingTexts = append(missingTexts, texts[index])
		missingHashes = append(missingHashes, key.ContentHash)
	}
	if len(invalidKeys) > 0 {
		keysToEvict := make([]Key, 0, len(invalidKeys))
		for _, key := range invalidKeys {
			keysToEvict = append(keysToEvict, key)
		}
		e.evictEmbeddingKeys(ctx, keysToEvict)
	}
	if len(missingTexts) > 0 {
		var computed [][]float32
		var computeErr error
		if usePool {
			computed, computeErr = e.inner.BatchEmbedWithPool(ctx, e.inner, missingTexts)
		} else {
			computed, computeErr = e.inner.BatchEmbed(ctx, missingTexts)
		}
		if computeErr != nil {
			return nil, computeErr
		}
		if len(computed) != len(missingTexts) {
			return nil, fmt.Errorf(
				"embedding cache: provider returned %d vectors for %d inputs",
				len(computed), len(missingTexts),
			)
		}
		toStore := make(map[Key][]byte, len(computed))
		for index, vector := range computed {
			if !vectorDimensionsValid(vector, e.inner.GetDimensions()) {
				return nil, fmt.Errorf("embedding cache: provider returned invalid vector dimensions")
			}
			hash := missingHashes[index]
			hits[hash] = encodeVector(vector)
			toStore[Key{
				TenantID: tenantID, Kind: KindEmbedding,
				ContentHash: hash, VersionHash: e.versionHash,
			}] = hits[hash]
		}
		if putErr := e.store.PutMany(ctx, toStore, embeddingCacheTTL, Reference{}); putErr != nil &&
			!errors.Is(putErr, ErrPayloadTooLarge) {
			logger.Warnf(ctx, "embedding cache persist failed; vectors remain usable: %v", putErr)
		}
	}
	for index, key := range keys {
		if results[index] != nil {
			continue
		}
		vector, decodeErr := decodeVector(hits[key.ContentHash])
		if decodeErr != nil {
			return nil, decodeErr
		}
		results[index] = vector
	}
	return results, nil
}

func (e *cachedEmbedder) evictEmbeddingKeys(ctx context.Context, keys []Key) {
	evictCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, key := range keys {
		if err := e.store.Evict(evictCtx, key); err != nil {
			logger.Warnf(ctx, "embedding cache eviction failed key=%s: %v", key.ContentHash, err)
		}
	}
}

func vectorDimensionsValid(vector []float32, expected int) bool {
	if len(vector) == 0 || (expected > 0 && len(vector) != expected) {
		return false
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}

func encodeVector(vector []float32) []byte {
	encoded := make([]byte, 4+len(vector)*4)
	binary.LittleEndian.PutUint32(encoded[:4], uint32(len(vector)))
	for index, value := range vector {
		binary.LittleEndian.PutUint32(encoded[4+index*4:], math.Float32bits(value))
	}
	return encoded
}

func decodeVector(encoded []byte) ([]float32, error) {
	if len(encoded) < 4 {
		return nil, errors.New("embedding cache vector is truncated")
	}
	dimensions := int(binary.LittleEndian.Uint32(encoded[:4]))
	if dimensions <= 0 || len(encoded) != 4+dimensions*4 {
		return nil, errors.New("embedding cache vector dimensions are invalid")
	}
	vector := make([]float32, dimensions)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(encoded[4+index*4:]))
	}
	return vector, nil
}

package contentcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(
		fmt.Sprintf("file:%s?mode=memory&cache=shared&_busy_timeout=5000", t.Name()),
	), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&entry{}, &reference{}))
	require.NoError(t, db.Exec(`
		CREATE TABLE knowledges (
			id TEXT PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			processing_generation TEXT NOT NULL,
			deleted_at DATETIME
		)
	`).Error)
	return NewStore(db), db
}

func TestStoreImmutableSharedEntryAndGenerationReferences(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()
	key := Key{
		TenantID: 7, Kind: KindEmbedding,
		ContentHash: Digest("same text"),
		VersionHash: Digest("model-v1"),
	}
	refA := Reference{KnowledgeID: "k-a", ProcessingGeneration: "g-a"}
	refB := Reference{KnowledgeID: "k-b", ProcessingGeneration: "g-b"}

	require.NoError(t, store.Put(ctx, key, []byte("payload"), time.Hour, refA))
	require.NoError(t, store.Put(ctx, key, []byte("payload"), time.Hour, refB))
	got, ok, err := store.Get(ctx, key, refA)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []byte("payload"), got)

	var row entry
	require.NoError(t, db.First(&row).Error)
	require.EqualValues(t, 2, row.RefCount)
	require.EqualValues(t, 1, row.HitCount)

	err = store.Put(ctx, key, []byte("different"), time.Hour, refA)
	require.ErrorIs(t, err, ErrImmutableKey)
}

func TestStoreDetectsCorruption(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()
	key := Key{
		TenantID: 7, Kind: KindParse,
		ContentHash: Digest("file"),
		VersionHash: Digest("parser"),
	}
	require.NoError(t, store.Put(ctx, key, []byte("valid"), time.Hour, Reference{}))
	require.NoError(t, db.Model(&entry{}).Where("tenant_id = ?", 7).
		UpdateColumn("payload_sha256", Digest("wrong")).Error)
	_, _, err := store.Get(ctx, key, Reference{})
	require.True(t, errors.Is(err, ErrCorruptPayload))
}

func TestStoreBoundsCorruptCompressedPayloadExpansion(t *testing.T) {
	store, db := newTestStore(t)
	store.maxPayloadBytes = 32
	key := Key{
		TenantID: 7, Kind: KindParse,
		ContentHash: Digest("compressed-bomb"),
		VersionHash: Digest("parser"),
	}
	plain := bytes.Repeat([]byte("a"), 4096)
	encoded, err := encodePayload(plain)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, db.Create(&entry{
		TenantID: key.TenantID, CacheKind: key.Kind,
		ContentHash: key.ContentHash, VersionHash: key.VersionHash,
		Payload: encoded, PayloadSHA256: payloadDigest(plain),
		PayloadSize: int64(len(plain)), LastAccessedAt: now,
		CreatedAt: now, UpdatedAt: now,
	}).Error)
	_, _, err = store.Get(context.Background(), key, Reference{})
	require.ErrorIs(t, err, ErrCorruptPayload)
}

func TestStoreTreatsInvalidJSONAsEvictableCorruption(t *testing.T) {
	store, _ := newTestStore(t)
	key := Key{
		TenantID: 7, Kind: KindGraph,
		ContentHash: Digest("invalid-json"),
		VersionHash: Digest("graph-v1"),
	}
	require.NoError(t, store.Put(
		context.Background(), key, []byte(`{"truncated":`), time.Hour, Reference{},
	))
	var decoded map[string]any
	hit, err := store.GetJSON(context.Background(), key, Reference{}, &decoded)
	require.False(t, hit)
	require.ErrorIs(t, err, ErrCorruptPayload)
}

func TestSweepDropsStaleGenerationReferenceThenEntry(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()
	key := Key{
		TenantID: 7, Kind: KindWikiMap,
		ContentHash: Digest("doc"),
		VersionHash: Digest("wiki-v1"),
	}
	ref := Reference{KnowledgeID: "k-a", ProcessingGeneration: "g-old"}
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges (id, tenant_id, processing_generation) VALUES ('k-a', 7, 'g-new')`,
	).Error)
	require.NoError(t, store.Put(ctx, key, []byte("wiki"), time.Hour, ref))
	require.NoError(t, db.Model(&entry{}).Where("tenant_id = ?", 7).
		UpdateColumn("last_accessed_at", time.Now().Add(-48*time.Hour)).Error)

	deleted, err := store.Sweep(ctx, time.Now().Add(-24*time.Hour), 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	var count int64
	require.NoError(t, db.Model(&reference{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Model(&entry{}).Count(&count).Error)
	require.Zero(t, count)
}

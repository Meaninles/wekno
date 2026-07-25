package contentcache

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	KindParse     = "parse"
	KindOCR       = "ocr"
	KindCaption   = "caption"
	KindEmbedding = "embedding"
	KindWikiMap   = "wiki_map"
	KindGraph     = "graph"

	defaultMaxPayloadBytes = 32 * 1024 * 1024
)

var (
	ErrPayloadTooLarge = errors.New("content cache payload exceeds configured limit")
	ErrCorruptPayload  = errors.New("content cache payload checksum mismatch")
	ErrImmutableKey    = errors.New("content cache immutable key collision")
)

type Key struct {
	TenantID    uint64
	Kind        string
	ContentHash string
	VersionHash string
}

type Reference struct {
	KnowledgeID          string
	ProcessingGeneration string
}

type entry struct {
	TenantID       uint64     `gorm:"primaryKey;column:tenant_id"`
	CacheKind      string     `gorm:"primaryKey;column:cache_kind"`
	ContentHash    string     `gorm:"primaryKey;column:content_hash"`
	VersionHash    string     `gorm:"primaryKey;column:version_hash"`
	Payload        []byte     `gorm:"column:payload"`
	PayloadSHA256  string     `gorm:"column:payload_sha256"`
	PayloadSize    int64      `gorm:"column:payload_size"`
	RefCount       int64      `gorm:"column:ref_count"`
	HitCount       int64      `gorm:"column:hit_count"`
	ExpiresAt      *time.Time `gorm:"column:expires_at"`
	LastAccessedAt time.Time  `gorm:"column:last_accessed_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

func (entry) TableName() string { return "custom_content_cache_entries" }

type reference struct {
	TenantID             uint64    `gorm:"primaryKey;column:tenant_id"`
	KnowledgeID          string    `gorm:"primaryKey;column:knowledge_id"`
	ProcessingGeneration string    `gorm:"primaryKey;column:processing_generation"`
	CacheKind            string    `gorm:"primaryKey;column:cache_kind"`
	ContentHash          string    `gorm:"primaryKey;column:content_hash"`
	VersionHash          string    `gorm:"primaryKey;column:version_hash"`
	CreatedAt            time.Time `gorm:"column:created_at"`
}

func (reference) TableName() string { return "custom_content_cache_refs" }

type Store struct {
	db              *gorm.DB
	maxPayloadBytes int
}

func NewStore(db *gorm.DB) *Store {
	maxBytes := defaultMaxPayloadBytes
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_CONTENT_CACHE_MAX_PAYLOAD_BYTES")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxBytes = parsed
		}
	}
	return &Store{db: db, maxPayloadBytes: maxBytes}
}

func Digest(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = io.WriteString(hash, strconv.Itoa(len(part)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, part)
		_, _ = io.WriteString(hash, "\x00")
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func DigestBytes(parts ...[]byte) string {
	hash := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(part)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func NormalizeContentHash(raw string) string {
	return Digest(strings.TrimSpace(strings.ToLower(raw)))
}

func (k Key) validate() error {
	if k.TenantID == 0 {
		return errors.New("content cache tenant ID is required")
	}
	switch k.Kind {
	case KindParse, KindOCR, KindCaption, KindEmbedding, KindWikiMap, KindGraph:
	default:
		return fmt.Errorf("content cache kind %q is unsupported", k.Kind)
	}
	if len(k.ContentHash) != sha256.Size*2 || len(k.VersionHash) != sha256.Size*2 {
		return errors.New("content cache hashes must be SHA-256 hex strings")
	}
	if _, err := hex.DecodeString(k.ContentHash); err != nil {
		return fmt.Errorf("invalid content hash: %w", err)
	}
	if _, err := hex.DecodeString(k.VersionHash); err != nil {
		return fmt.Errorf("invalid version hash: %w", err)
	}
	return nil
}

func validReference(ref Reference) bool {
	return strings.TrimSpace(ref.KnowledgeID) != "" &&
		strings.TrimSpace(ref.ProcessingGeneration) != ""
}

func payloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// encodePayload adds a one-byte codec marker. Small payloads remain raw;
// larger payloads use gzip so PostgreSQL and network I/O stay bounded.
func encodePayload(payload []byte) ([]byte, error) {
	if len(payload) < 1024 {
		return append([]byte{0}, payload...), nil
	}
	var buffer bytes.Buffer
	buffer.WriteByte(1)
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func decodePayload(encoded []byte, maxPayloadBytes int) ([]byte, error) {
	if len(encoded) == 0 {
		return nil, errors.New("content cache encoded payload is empty")
	}
	if maxPayloadBytes <= 0 {
		maxPayloadBytes = defaultMaxPayloadBytes
	}
	switch encoded[0] {
	case 0:
		if len(encoded)-1 > maxPayloadBytes {
			return nil, ErrPayloadTooLarge
		}
		return append([]byte(nil), encoded[1:]...), nil
	case 1:
		reader, err := gzip.NewReader(bytes.NewReader(encoded[1:]))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		decoded, err := io.ReadAll(io.LimitReader(reader, int64(maxPayloadBytes)+1))
		if err != nil {
			return nil, err
		}
		if len(decoded) > maxPayloadBytes {
			return nil, ErrPayloadTooLarge
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("content cache codec %d is unsupported", encoded[0])
	}
}

func (s *Store) Get(ctx context.Context, key Key, ref Reference) ([]byte, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, nil
	}
	if err := key.validate(); err != nil {
		return nil, false, err
	}

	var payload []byte
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var found entry
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
				key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
			).
			Take(&found)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if result.Error != nil {
			return result.Error
		}
		now := time.Now()
		if found.ExpiresAt != nil && !found.ExpiresAt.After(now) {
			return nil
		}
		decoded, err := decodePayload(found.Payload, s.maxPayloadBytes)
		if err != nil {
			return fmt.Errorf("%w: decode content cache payload: %v", ErrCorruptPayload, err)
		}
		if int64(len(decoded)) != found.PayloadSize || payloadDigest(decoded) != found.PayloadSHA256 {
			return ErrCorruptPayload
		}
		if validReference(ref) {
			inserted, err := attachReference(tx, key, ref, now)
			if err != nil {
				return err
			}
			if inserted {
				if err := updateEntryCounter(tx, key, "ref_count", 1); err != nil {
					return err
				}
			}
		}
		if err := tx.Model(&entry{}).
			Where(
				"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
				key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
			).
			Updates(map[string]any{
				"hit_count":        gorm.Expr("hit_count + 1"),
				"last_accessed_at": now,
				"updated_at":       now,
			}).Error; err != nil {
			return err
		}
		payload = decoded
		return nil
	})
	if err != nil {
		pipelineobs.ObserveContentCache(key.Kind, "get", "error", -1)
		return nil, false, err
	}
	if payload == nil {
		pipelineobs.ObserveContentCache(key.Kind, "get", "miss", -1)
	} else {
		pipelineobs.ObserveContentCache(key.Kind, "get", "hit", len(payload))
	}
	return payload, payload != nil, nil
}

// GetMany is the batch-oriented read path used by embedding. All keys must
// share a tenant, kind and version; this turns hundreds of per-chunk lookups
// into one indexed SELECT and one hit-counter UPDATE.
func (s *Store) GetMany(
	ctx context.Context,
	keys []Key,
	ref Reference,
) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))
	if s == nil || s.db == nil || len(keys) == 0 {
		return out, nil
	}
	first := keys[0]
	if err := first.validate(); err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if err := key.validate(); err != nil {
			return nil, err
		}
		if key.TenantID != first.TenantID || key.Kind != first.Kind || key.VersionHash != first.VersionHash {
			return nil, errors.New("content cache batch keys must share tenant, kind and version")
		}
		if _, exists := seen[key.ContentHash]; exists {
			continue
		}
		seen[key.ContentHash] = struct{}{}
		hashes = append(hashes, key.ContentHash)
	}
	now := time.Now()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []entry
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"tenant_id = ? AND cache_kind = ? AND version_hash = ? AND content_hash IN ? AND (expires_at IS NULL OR expires_at > ?)",
				first.TenantID, first.Kind, first.VersionHash, hashes, now,
			).
			Find(&rows).Error; err != nil {
			return err
		}
		hitHashes := make([]string, 0, len(rows))
		for _, row := range rows {
			decoded, err := decodePayload(row.Payload, s.maxPayloadBytes)
			if err != nil {
				return fmt.Errorf("%w: decode content cache payload: %v", ErrCorruptPayload, err)
			}
			if int64(len(decoded)) != row.PayloadSize || payloadDigest(decoded) != row.PayloadSHA256 {
				return ErrCorruptPayload
			}
			out[row.ContentHash] = decoded
			hitHashes = append(hitHashes, row.ContentHash)
			if validReference(ref) {
				key := Key{
					TenantID: row.TenantID, Kind: row.CacheKind,
					ContentHash: row.ContentHash, VersionHash: row.VersionHash,
				}
				inserted, err := attachReference(tx, key, ref, now)
				if err != nil {
					return err
				}
				if inserted {
					if err := updateEntryCounter(tx, key, "ref_count", 1); err != nil {
						return err
					}
				}
			}
		}
		if len(hitHashes) == 0 {
			return nil
		}
		return tx.Model(&entry{}).
			Where(
				"tenant_id = ? AND cache_kind = ? AND version_hash = ? AND content_hash IN ?",
				first.TenantID, first.Kind, first.VersionHash, hitHashes,
			).
			Updates(map[string]any{
				"hit_count":        gorm.Expr("hit_count + 1"),
				"last_accessed_at": now,
				"updated_at":       now,
			}).Error
	})
	result := "miss"
	if err != nil {
		result = "error"
	} else if len(out) == len(seen) {
		result = "hit"
	} else if len(out) > 0 {
		result = "partial"
	}
	pipelineobs.ObserveContentCache(first.Kind, "get_many", result, -1)
	return out, err
}

func (s *Store) Put(
	ctx context.Context,
	key Key,
	payload []byte,
	ttl time.Duration,
	ref Reference,
) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := key.validate(); err != nil {
		return err
	}
	if len(payload) == 0 {
		return errors.New("content cache payload is empty")
	}
	if len(payload) > s.maxPayloadBytes {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(payload), s.maxPayloadBytes)
	}
	encoded, err := encodePayload(payload)
	if err != nil {
		return err
	}
	now := time.Now()
	var expiresAt *time.Time
	if ttl > 0 {
		value := now.Add(ttl)
		expiresAt = &value
	}
	checksum := payloadDigest(payload)

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate := entry{
			TenantID: key.TenantID, CacheKind: key.Kind,
			ContentHash: key.ContentHash, VersionHash: key.VersionHash,
			Payload: encoded, PayloadSHA256: checksum, PayloadSize: int64(len(payload)),
			ExpiresAt: expiresAt, LastAccessedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var existing entry
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
					key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
				).
				Take(&existing).Error; err != nil {
				return err
			}
			if existing.PayloadSHA256 != checksum || existing.PayloadSize != int64(len(payload)) {
				return ErrImmutableKey
			}
			updates := map[string]any{"last_accessed_at": now, "updated_at": now}
			if existing.ExpiresAt == nil || (expiresAt != nil && expiresAt.After(*existing.ExpiresAt)) {
				updates["expires_at"] = expiresAt
			}
			if err := tx.Model(&entry{}).
				Where(
					"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
					key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
				).
				Updates(updates).Error; err != nil {
				return err
			}
		}
		if validReference(ref) {
			inserted, err := attachReference(tx, key, ref, now)
			if err != nil {
				return err
			}
			if inserted {
				return updateEntryCounter(tx, key, "ref_count", 1)
			}
		}
		return nil
	})
	result := "success"
	if err != nil {
		result = "error"
	}
	pipelineobs.ObserveContentCache(key.Kind, "put", result, len(payload))
	return err
}

// PutMany writes immutable entries in one transaction. Existing entries are
// checksum-verified and have their TTL extended; a conflicting payload for the
// same content/version key is rejected rather than silently replacing data.
func (s *Store) PutMany(
	ctx context.Context,
	values map[Key][]byte,
	ttl time.Duration,
	ref Reference,
) error {
	if s == nil || s.db == nil || len(values) == 0 {
		return nil
	}
	now := time.Now()
	var expiresAt *time.Time
	if ttl > 0 {
		value := now.Add(ttl)
		expiresAt = &value
	}
	candidates := make([]entry, 0, len(values))
	for key, payload := range values {
		if err := key.validate(); err != nil {
			return err
		}
		if len(payload) == 0 {
			return errors.New("content cache payload is empty")
		}
		if len(payload) > s.maxPayloadBytes {
			return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(payload), s.maxPayloadBytes)
		}
		encoded, err := encodePayload(payload)
		if err != nil {
			return err
		}
		candidates = append(candidates, entry{
			TenantID: key.TenantID, CacheKind: key.Kind,
			ContentHash: key.ContentHash, VersionHash: key.VersionHash,
			Payload: encoded, PayloadSHA256: payloadDigest(payload), PayloadSize: int64(len(payload)),
			ExpiresAt: expiresAt, LastAccessedAt: now, CreatedAt: now, UpdatedAt: now,
		})
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			CreateInBatches(candidates, 200).Error; err != nil {
			return err
		}
		if !validReference(ref) {
			// Embedding is the dominant batch caller and does not attach
			// per-document references. Validate and refresh the complete batch
			// with two set-oriented statements instead of issuing a SELECT and
			// UPDATE for every vector.
			tuples := make([][]any, 0, len(candidates))
			for _, candidate := range candidates {
				tuples = append(tuples, []any{
					candidate.TenantID,
					candidate.CacheKind,
					candidate.ContentHash,
					candidate.VersionHash,
				})
			}
			var existingRows []entry
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"(tenant_id, cache_kind, content_hash, version_hash) IN ?",
					tuples,
				).
				Find(&existingRows).Error; err != nil {
				return err
			}
			if len(existingRows) != len(candidates) {
				return errors.New("content cache batch validation returned incomplete rows")
			}
			expected := make(map[Key]entry, len(candidates))
			for _, candidate := range candidates {
				expected[Key{
					TenantID: candidate.TenantID, Kind: candidate.CacheKind,
					ContentHash: candidate.ContentHash, VersionHash: candidate.VersionHash,
				}] = candidate
			}
			for _, existing := range existingRows {
				key := Key{
					TenantID: existing.TenantID, Kind: existing.CacheKind,
					ContentHash: existing.ContentHash, VersionHash: existing.VersionHash,
				}
				candidate, ok := expected[key]
				if !ok ||
					existing.PayloadSHA256 != candidate.PayloadSHA256 ||
					existing.PayloadSize != candidate.PayloadSize {
					return ErrImmutableKey
				}
			}
			updates := map[string]any{
				"last_accessed_at": now,
				"updated_at":       now,
			}
			if expiresAt != nil {
				updates["expires_at"] = gorm.Expr(
					"CASE WHEN expires_at IS NULL OR expires_at < ? THEN ? ELSE expires_at END",
					*expiresAt,
					*expiresAt,
				)
			}
			return tx.Model(&entry{}).
				Where(
					"(tenant_id, cache_kind, content_hash, version_hash) IN ?",
					tuples,
				).
				Updates(updates).Error
		}
		for _, candidate := range candidates {
			key := Key{
				TenantID: candidate.TenantID, Kind: candidate.CacheKind,
				ContentHash: candidate.ContentHash, VersionHash: candidate.VersionHash,
			}
			var existing entry
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(
					"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
					key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
				).
				Take(&existing).Error; err != nil {
				return err
			}
			if existing.PayloadSHA256 != candidate.PayloadSHA256 ||
				existing.PayloadSize != candidate.PayloadSize {
				return ErrImmutableKey
			}
			updates := map[string]any{"last_accessed_at": now, "updated_at": now}
			if existing.ExpiresAt == nil || (expiresAt != nil && expiresAt.After(*existing.ExpiresAt)) {
				updates["expires_at"] = expiresAt
			}
			if err := tx.Model(&entry{}).
				Where(
					"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
					key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
				).
				Updates(updates).Error; err != nil {
				return err
			}
			if validReference(ref) {
				inserted, err := attachReference(tx, key, ref, now)
				if err != nil {
					return err
				}
				if inserted {
					if err := updateEntryCounter(tx, key, "ref_count", 1); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	result := "success"
	if err != nil {
		result = "error"
	}
	kind := ""
	for key := range values {
		kind = key.Kind
		break
	}
	pipelineobs.ObserveContentCache(kind, "put_many", result, -1)
	return err
}

func (s *Store) GetJSON(ctx context.Context, key Key, ref Reference, target any) (bool, error) {
	payload, ok, err := s.Get(ctx, key, ref)
	if err != nil || !ok {
		return ok, err
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return false, fmt.Errorf("%w: decode content cache JSON: %v", ErrCorruptPayload, err)
	}
	return true, nil
}

func (s *Store) PutJSON(
	ctx context.Context,
	key Key,
	value any,
	ttl time.Duration,
	ref Reference,
) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode content cache JSON: %w", err)
	}
	return s.Put(ctx, key, payload, ttl, ref)
}

func (s *Store) Evict(ctx context.Context, key Key) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := key.validate(); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(
			"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
			key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
		).Delete(&reference{}).Error; err != nil {
			return err
		}
		return tx.Where(
			"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
			key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
		).Delete(&entry{}).Error
	})
}

func attachReference(tx *gorm.DB, key Key, ref Reference, now time.Time) (bool, error) {
	row := reference{
		TenantID: key.TenantID, KnowledgeID: ref.KnowledgeID,
		ProcessingGeneration: ref.ProcessingGeneration,
		CacheKind:            key.Kind, ContentHash: key.ContentHash, VersionHash: key.VersionHash,
		CreatedAt: now,
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	return result.RowsAffected == 1, result.Error
}

func updateEntryCounter(tx *gorm.DB, key Key, column string, delta int64) error {
	if column != "ref_count" && column != "hit_count" {
		return errors.New("unsupported content cache counter")
	}
	return tx.Model(&entry{}).
		Where(
			"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
			key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
		).
		UpdateColumn(column, gorm.Expr(column+" + ?", delta)).Error
}

// Sweep removes references whose knowledge generation no longer exists, then
// deletes old unreferenced immutable entries. It is intentionally bounded so a
// maintenance pass cannot monopolize the database.
func (s *Store) Sweep(ctx context.Context, unusedBefore time.Time, limit int) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 500
	}
	var deleted int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var staleRefs []reference
		if err := tx.Raw(`
			SELECT r.*
			FROM custom_content_cache_refs r
			LEFT JOIN knowledges k
			  ON k.tenant_id = r.tenant_id
			 AND k.id = r.knowledge_id
			 AND k.processing_generation = r.processing_generation
			 AND k.deleted_at IS NULL
			WHERE k.id IS NULL
			LIMIT ?
		`, limit).Scan(&staleRefs).Error; err != nil {
			return err
		}
		for _, ref := range staleRefs {
			key := Key{
				TenantID: ref.TenantID, Kind: ref.CacheKind,
				ContentHash: ref.ContentHash, VersionHash: ref.VersionHash,
			}
			result := tx.Where(
				"tenant_id = ? AND knowledge_id = ? AND processing_generation = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ?",
				ref.TenantID, ref.KnowledgeID, ref.ProcessingGeneration,
				ref.CacheKind, ref.ContentHash, ref.VersionHash,
			).Delete(&reference{})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected > 0 {
				if err := tx.Model(&entry{}).
					Where(
						"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ? AND ref_count > 0",
						key.TenantID, key.Kind, key.ContentHash, key.VersionHash,
					).
					UpdateColumn("ref_count", gorm.Expr("ref_count - 1")).Error; err != nil {
					return err
				}
			}
		}

		var victims []entry
		now := time.Now()
		if err := tx.Where(
			"ref_count = 0 AND (last_accessed_at < ? OR (expires_at IS NOT NULL AND expires_at <= ?))",
			unusedBefore, now,
		).Order("last_accessed_at ASC").Limit(limit).Find(&victims).Error; err != nil {
			return err
		}
		for _, victim := range victims {
			result := tx.Where(
				"tenant_id = ? AND cache_kind = ? AND content_hash = ? AND version_hash = ? AND ref_count = 0",
				victim.TenantID, victim.CacheKind, victim.ContentHash, victim.VersionHash,
			).Delete(&entry{})
			if result.Error != nil {
				return result.Error
			}
			deleted += result.RowsAffected
		}
		return nil
	})
	return deleted, err
}

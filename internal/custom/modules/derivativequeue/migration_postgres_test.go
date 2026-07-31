package derivativequeue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

func openDerivativeMigrationPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("WEKNORA_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("WEKNORA_TEST_POSTGRES_DSN is not configured")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	schema := "derivativequeue_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	t.Cleanup(func() {
		require.NoError(t, admin.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	})
	if strings.Contains(dsn, "://") {
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		dsn += separator + "search_path=" + schema
	} else {
		dsn += " search_path=" + schema
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	return db
}

func TestPostgresMigrateProviderCallsUpgradesOldConstraintWithoutDataLoss(t *testing.T) {
	db := openDerivativeMigrationPostgres(t)
	require.NoError(t, db.AutoMigrate(&WorkItem{}, &Result{}))
	now := time.Now().UTC()
	item := WorkItem{
		ID: uuid.NewString(), TenantID: 7, KnowledgeBaseID: "kb-1",
		KnowledgeID: "knowledge-1", ProcessingGeneration: "generation-2",
		ProcessingAttempt: 2, ItemID: "question:0", WorkKind: WorkQuestion,
		Payload: types.JSON(`{"chunk_ids":["chunk-1"]}`), PayloadHash: "payload",
		State: StateProviderRunning, NextAttemptAt: now, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(&item).Error)
	require.NoError(t, db.Exec(`
		CREATE TABLE custom_derivative_provider_calls (
			id uuid PRIMARY KEY,
			work_item_id uuid NOT NULL,
			request_hash varchar(64) NOT NULL,
			provider_request_key varchar(190) NOT NULL UNIQUE,
			model_id varchar(64) NOT NULL DEFAULT '',
			response jsonb NOT NULL,
			response_size bigint NOT NULL DEFAULT 0,
			content_checksum varchar(64) NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			CONSTRAINT uq_custom_derivative_provider_call
				UNIQUE (work_item_id, request_hash)
		)`).Error)
	require.NoError(t, db.Exec(`
		CREATE UNIQUE INDEX uq_derivative_provider_call
		ON custom_derivative_provider_calls (work_item_id, request_hash)`).Error)
	callID := uuid.NewString()
	require.NoError(t, db.Exec(`
		INSERT INTO custom_derivative_provider_calls
			(id, work_item_id, request_hash, provider_request_key, response, content_checksum)
		VALUES (?, ?, 'same-request', 'request-attempt-1', '{"content":"invalid"}', 'checksum-1')`,
		callID, item.ID).Error)

	repository := NewRepository(db)
	require.NoError(t, repository.Migrate(context.Background()))
	require.NoError(t, repository.Migrate(context.Background()), "migration must be idempotent")

	var preserved ProviderCall
	require.NoError(t, db.Where("id = ?", callID).Take(&preserved).Error)
	require.Equal(t, 1, preserved.Attempt)
	require.Equal(t, item.ProcessingGeneration, preserved.ProcessingGeneration)
	require.Equal(t, ProviderCallCheckpointed, preserved.Disposition)

	second := ProviderCall{
		ID: uuid.NewString(), WorkItemID: item.ID, RequestHash: "same-request",
		Attempt: 2, ProviderRequestKey: "request-attempt-2", ModelID: "model-1",
		Response: types.JSON(`{"content":"valid"}`), ContentChecksum: "checksum-2",
		ProcessingGeneration: item.ProcessingGeneration,
		Disposition:          ProviderCallAccepted, CreatedAt: now,
	}
	require.NoError(t, db.Create(&second).Error,
		"same request hash must accept durable attempt N+1")

	duplicate := second
	duplicate.ID = uuid.NewString()
	duplicate.ProviderRequestKey = "request-attempt-2-duplicate"
	require.Error(t, db.Create(&duplicate).Error,
		"same work item, request hash, and attempt must remain unique")

	invalid := second
	invalid.ID = uuid.NewString()
	invalid.Attempt = 3
	invalid.ProviderRequestKey = "request-invalid-disposition"
	invalid.Disposition = "unknown"
	require.Error(t, db.Create(&invalid).Error,
		"database must reject unknown provider-call dispositions")

	var definition string
	require.NoError(t, db.Raw(`
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'custom_derivative_provider_calls'::regclass
		  AND conname = 'uq_custom_derivative_provider_call'`).Scan(&definition).Error)
	require.Equal(t, "UNIQUE (work_item_id, request_hash, attempt)", definition)

	var legacyIndexes int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*) FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND indexname = 'uq_derivative_provider_call'`).Scan(&legacyIndexes).Error)
	require.Zero(t, legacyIndexes,
		"the old two-column GORM index must not survive the executable migration")

	var replayIndexes int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*) FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND indexname IN (
			'idx_custom_derivative_provider_calls_replay',
			'idx_custom_derivative_provider_calls_generation'
		  )`).Scan(&replayIndexes).Error)
	require.EqualValues(t, 2, replayIndexes, fmt.Sprintf("indexes=%d", replayIndexes))
}

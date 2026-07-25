package wikiingestguard

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openGuardTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
CREATE TABLE knowledges (
  id TEXT PRIMARY KEY,
  tenant_id INTEGER NOT NULL,
  knowledge_base_id TEXT NOT NULL,
  processing_generation TEXT NOT NULL,
  parse_status TEXT NOT NULL,
  processed_at DATETIME,
  wiki_status TEXT NOT NULL DEFAULT 'pending',
  deleted_at DATETIME
);
CREATE TABLE task_pending_ops (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id INTEGER NOT NULL,
  task_type TEXT NOT NULL,
  scope TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  op TEXT NOT NULL,
  dedup_key TEXT NOT NULL DEFAULT '',
  payload JSON NOT NULL,
  fail_count INTEGER NOT NULL DEFAULT 0,
  enqueued_at DATETIME NOT NULL,
  claimed_at DATETIME
);`).Error)
	return db
}

func insertGuardKnowledge(t *testing.T, db *gorm.DB, generation, status string) {
	t.Helper()
	processedAt := time.Now().UTC()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges
         (id, tenant_id, knowledge_base_id, processing_generation, parse_status, processed_at)
         VALUES (?, ?, ?, ?, ?, ?)`,
		"knowledge-1", 42, "kb-1", generation, status, processedAt,
	).Error)
}

func TestValidateRejectsGenerationChangedBeforeCommit(t *testing.T) {
	db := openGuardTestDB(t)
	insertGuardKnowledge(t, db, "generation-2", types.ParseStatusCompleted)
	identity := Identity{
		TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1",
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		return Validate(WithValidation(context.Background(), identity), tx)
	})
	var stale *StaleIdentityError
	require.ErrorAs(t, err, &stale)
	require.Equal(t, []Identity{identity}, stale.Identities)
}

func TestValidateDoesNotTerminallyAcknowledgeIncompleteCurrentGeneration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      string
		uncommitted bool
	}{
		{name: "processed_at missing", status: types.ParseStatusProcessing, uncommitted: true},
		{name: "current generation returned to pending", status: types.ParseStatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openGuardTestDB(t)
			insertGuardKnowledge(t, db, "generation-1", tc.status)
			if tc.uncommitted {
				require.NoError(t, db.Exec(
					"UPDATE knowledges SET processed_at = NULL WHERE id = ?", "knowledge-1",
				).Error)
			}
			identity := Identity{
				TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
				ProcessingGeneration: "generation-1",
			}

			err := db.Transaction(func(tx *gorm.DB) error {
				return Validate(WithValidation(context.Background(), identity), tx)
			})
			require.Error(t, err)
			require.Empty(t, StaleIdentities(err),
				"incomplete current work must retry instead of being terminally acknowledged")
		})
	}
}

func TestValidateTerminallyAcknowledgesSettledWikiGeneration(t *testing.T) {
	for _, status := range []string{
		types.WikiStatusCompleted,
		types.WikiStatusDegraded,
		types.WikiStatusFailed,
	} {
		t.Run(status, func(t *testing.T) {
			db := openGuardTestDB(t)
			insertGuardKnowledge(t, db, "generation-1", types.ParseStatusFinalizing)
			require.NoError(t, db.Exec(
				"UPDATE knowledges SET wiki_status = ? WHERE id = ?",
				status, "knowledge-1",
			).Error)
			identity := Identity{
				TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
				ProcessingGeneration: "generation-1",
			}

			err := db.Transaction(func(tx *gorm.DB) error {
				return Validate(WithValidation(context.Background(), identity), tx)
			})
			require.Equal(t, []Identity{identity}, StaleIdentities(err))
		})
	}
}

func TestRecordPageApplicationMergesCheckpointWithoutLosingPreparedPlan(t *testing.T) {
	db := openGuardTestDB(t)
	insertGuardKnowledge(t, db, "generation-1", types.ParseStatusCompleted)
	payload := json.RawMessage(`{
      "op":"ingest",
      "knowledge_id":"knowledge-1",
      "processing_generation":"generation-1",
      "prepared":{"doc_title":"kept","updates":[{"Slug":"entity/acme"}]}
    }`)
	row := &types.TaskPendingOp{
		TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb-1", Op: "ingest", DedupKey: "knowledge-1:generation-1",
		Payload: payload, EnqueuedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(row).Error)
	identity := Identity{
		TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1",
	}
	ctx := WithPageApplication(context.Background(), "entity/acme", Operation{
		PendingOpID: row.ID,
		Identity:    identity,
	})

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := Validate(ctx, tx); err != nil {
			return err
		}
		return RecordPageApplication(ctx, tx, "entity/acme")
	}))
	// Repeating the same commit is idempotent.
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := Validate(ctx, tx); err != nil {
			return err
		}
		return RecordPageApplication(ctx, tx, "entity/acme")
	}))

	var stored types.TaskPendingOp
	require.NoError(t, db.First(&stored, row.ID).Error)
	var decoded struct {
		Applied  []string `json:"applied_page_slugs"`
		Prepared struct {
			DocTitle string `json:"doc_title"`
		} `json:"prepared"`
	}
	require.NoError(t, json.Unmarshal(stored.Payload, &decoded))
	require.Equal(t, []string{"entity/acme"}, decoded.Applied)
	require.Equal(t, "kept", decoded.Prepared.DocTitle)
}

func TestRecordPageApplicationMissingPendingRowIsTerminalAndRollsBack(t *testing.T) {
	db := openGuardTestDB(t)
	insertGuardKnowledge(t, db, "generation-1", types.ParseStatusCompleted)
	identity := Identity{
		TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1",
	}
	ctx := WithPageApplication(context.Background(), "entity/acme", Operation{
		PendingOpID: 999,
		Identity:    identity,
	})

	err := db.Transaction(func(tx *gorm.DB) error {
		return RecordPageApplication(ctx, tx, "entity/acme")
	})
	require.NotNil(t, err)
	require.NotEmpty(t, StaleIdentities(err))
	require.False(t, errors.Is(err, ErrInvalidIdentity))
}

func TestRecordPageApplicationRequiresDurablePreparedPlan(t *testing.T) {
	db := openGuardTestDB(t)
	insertGuardKnowledge(t, db, "generation-1", types.ParseStatusCompleted)
	row := &types.TaskPendingOp{
		TenantID: 42, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb-1", Op: "ingest", DedupKey: "knowledge-1:generation-1",
		Payload: json.RawMessage(`{
			"op":"ingest",
			"knowledge_id":"knowledge-1",
			"processing_generation":"generation-1"
		}`),
		EnqueuedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(row).Error)
	identity := Identity{
		TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1",
	}
	ctx := WithPageApplication(context.Background(), "entity/acme", Operation{
		PendingOpID: row.ID,
		Identity:    identity,
	})

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := Validate(ctx, tx); err != nil {
			return err
		}
		return RecordPageApplication(ctx, tx, "entity/acme")
	})
	require.ErrorContains(t, err, "no durable prepared plan")
	require.Empty(t, StaleIdentities(err), "an incomplete checkpoint must retry, not be terminally acknowledged")
}

func TestValidateScopePostgresLocksLeaseBeforeKnowledge(t *testing.T) {
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{DisableAutomaticPing: true})
	require.NoError(t, err)

	lease := wikilease.Identity{
		TenantID: 42, KnowledgeBaseID: "kb-1", Epoch: 9,
		Token: "0123456789012345678901234567890123456789012",
	}
	identity := Identity{
		TenantID: 42, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1",
	}
	ctx := WithValidation(wikilease.WithIdentity(context.Background(), lease), identity)

	// sqlmock is ordered by default. The sequence therefore proves the inner
	// lock contract: lease SHARE precedes knowledge SHARE. Repository callers
	// acquire the parent KB lock before entering this function.
	mock.ExpectQuery(`SELECT \* FROM "custom_wiki_ingest_leases" WHERE tenant_id = \$1 AND knowledge_base_id = \$2 LIMIT \$3 FOR SHARE`).
		WithArgs(uint64(42), "kb-1", 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"tenant_id", "knowledge_base_id", "epoch", "lease_token", "acquired_at", "updated_at",
		}).AddRow(42, "kb-1", 9, lease.Token, time.Now(), time.Now()))
	mock.ExpectQuery(`SELECT "id","tenant_id","knowledge_base_id","processing_generation","parse_status","processed_at","wiki_status","deleted_at" FROM "knowledges" WHERE id = \$1 LIMIT \$2 FOR SHARE`).
		WithArgs("knowledge-1", 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "tenant_id", "knowledge_base_id", "processing_generation", "parse_status", "processed_at", "wiki_status", "deleted_at",
		}).AddRow("knowledge-1", 42, "kb-1", "generation-1", types.ParseStatusCompleted, time.Now(), types.WikiStatusPending, nil))
	require.NoError(t, ValidateScope(ctx, db, 42, "kb-1"))
	require.NoError(t, mock.ExpectationsWereMet())
}

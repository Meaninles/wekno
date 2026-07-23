package wikilease

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openLeaseSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.KnowledgeBase{}, &types.TaskPendingOp{}, &Lease{}))
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	return db
}

func TestAcquireAdvancesEpochAndFencesFormerOwnerSQLite(t *testing.T) {
	db := openLeaseSQLite(t)
	first, err := Acquire(context.Background(), db, 7, "kb-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, first.Epoch)
	require.Len(t, first.Token, 43)

	firstCtx := WithIdentity(context.Background(), first)
	require.NoError(t, kbwritefence.WithActive(firstCtx, db, 7, "kb-1", func(tx *gorm.DB) error {
		return Validate(firstCtx, tx, 7, "kb-1")
	}))

	second, err := Acquire(context.Background(), db, 7, "kb-1")
	require.NoError(t, err)
	require.EqualValues(t, 2, second.Epoch)
	require.NotEqual(t, first.Token, second.Token)

	err = kbwritefence.WithActive(firstCtx, db, 7, "kb-1", func(tx *gorm.DB) error {
		return Validate(firstCtx, tx, 7, "kb-1")
	})
	var fenced *FencedError
	require.ErrorAs(t, err, &fenced)
	require.ErrorIs(t, err, ErrFenced)
	require.EqualValues(t, 2, fenced.CurrentEpoch)

	secondCtx := WithIdentity(context.Background(), second)
	require.NoError(t, kbwritefence.WithActive(secondCtx, db, 7, "kb-1", func(tx *gorm.DB) error {
		return Validate(secondCtx, tx, 7, "kb-1")
	}))
}

func TestValidatePolicyAdminCompatibleButDurableMissingIdentityFailsClosed(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	// No lease table exists. An ordinary admin/direct call must remain a true
	// no-op and therefore cannot accidentally query the missing table.
	require.NoError(t, Validate(context.Background(), db, 7, "kb-1"))
	require.ErrorIs(t, Validate(Require(context.Background()), db, 7, "kb-1"), ErrLeaseRequired)
}

func TestAcquireDoesNotRecreateLeaseAfterTombstoneQueueDrained(t *testing.T) {
	db := openLeaseSQLite(t)
	require.NoError(t, db.Model(&types.KnowledgeBase{}).
		Where("id = ? AND tenant_id = ?", "kb-1", 7).
		Update("deleted_at", gorm.Expr("CURRENT_TIMESTAMP")).Error)

	_, err := Acquire(context.Background(), db, 7, "kb-1")
	require.ErrorIs(t, err, ErrTombstoneDrained)

	pending := &types.TaskPendingOp{
		TenantID: 7, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb-1", Op: "ingest", DedupKey: "knowledge-1:generation-1",
		Payload: []byte(`{"knowledge_id":"knowledge-1"}`),
	}
	require.NoError(t, db.Create(pending).Error)
	lease, err := Acquire(context.Background(), db, 7, "kb-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, lease.Epoch)
	require.NoError(t, db.Delete(pending).Error)

	_, err = Acquire(context.Background(), db, 7, "kb-1")
	require.ErrorIs(t, err, ErrTombstoneDrained)
	var count int64
	require.NoError(t, db.Model(&Lease{}).Count(&count).Error)
	require.EqualValues(t, 1, count, "terminal no-op must not create or advance another lease")
}

func TestValidatePostgresUsesShareLock(t *testing.T) {
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{DisableAutomaticPing: true})
	require.NoError(t, err)

	identity := Identity{TenantID: 7, KnowledgeBaseID: "kb-1", Epoch: 4, Token: "0123456789012345678901234567890123456789012"}
	mock.ExpectQuery(`SELECT \* FROM "custom_wiki_ingest_leases" WHERE tenant_id = \$1 AND knowledge_base_id = \$2 LIMIT \$3 FOR SHARE`).
		WithArgs(uint64(7), "kb-1", 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"tenant_id", "knowledge_base_id", "epoch", "lease_token", "acquired_at", "updated_at",
		}).AddRow(7, "kb-1", 4, identity.Token, time.Now(), time.Now()))
	require.NoError(t, Validate(WithIdentity(context.Background(), identity), db, 7, "kb-1"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAcquirePostgresLocksKBThenLeaseAndAdvancesEpoch(t *testing.T) {
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{DisableAutomaticPing: true})
	require.NoError(t, err)
	now := time.Now()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT "id","tenant_id","deleted_at" FROM "knowledge_bases" WHERE id = \$1 AND tenant_id = \$2 LIMIT \$3 FOR UPDATE`).
		WithArgs("kb-1", uint64(7), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "deleted_at"}).AddRow("kb-1", 7, nil))
	mock.ExpectQuery(`SELECT \* FROM "custom_wiki_ingest_leases" WHERE tenant_id = \$1 AND knowledge_base_id = \$2 LIMIT \$3 FOR UPDATE`).
		WithArgs(uint64(7), "kb-1", 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"tenant_id", "knowledge_base_id", "epoch", "lease_token", "acquired_at", "updated_at",
		}).AddRow(7, "kb-1", 4, "old-token-012345678901234567890123456789", now, now))
	mock.ExpectExec(`UPDATE "custom_wiki_ingest_leases" SET .* WHERE tenant_id = \$[0-9]+ AND knowledge_base_id = \$[0-9]+ AND epoch = \$[0-9]+`).
		WithArgs(sqlmock.AnyArg(), int64(5), sqlmock.AnyArg(), sqlmock.AnyArg(), uint64(7), "kb-1", int64(4)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	identity, err := Acquire(context.Background(), db, 7, "kb-1")
	require.NoError(t, err)
	require.EqualValues(t, 5, identity.Epoch)
	require.Len(t, identity.Token, 43)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFencedErrorDoesNotExposeToken(t *testing.T) {
	err := (&FencedError{
		ExpectedTenantID: 7, ExpectedKnowledgeBaseID: "kb-1", ExpectedEpoch: 1,
		CurrentEpoch: 2, Reason: "replacement",
	}).Error()
	require.NotContains(t, err, "secret-token")
	require.True(t, errors.Is(&FencedError{}, ErrFenced))
}

package kbwritefence

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestLockActiveSharedPostgresUsesCompatibleForShareLock(t *testing.T) {
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		DisableAutomaticPing: true,
	})
	require.NoError(t, err)

	mock.ExpectQuery(
		`SELECT "id","tenant_id","deleted_at" FROM "knowledge_bases" WHERE id = \$1 AND tenant_id = \$2 LIMIT \$3 FOR SHARE`,
	).WithArgs("kb-1", uint64(7), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "deleted_at"}).AddRow("kb-1", 7, nil))
	require.NoError(t, LockActiveShared(db, 7, "kb-1"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithActiveSharedBlocksSQLiteDeleteAndRejectsAfterTombstone(t *testing.T) {
	dsn := "file:" + t.TempDir() + `/shared-fence.db?_busy_timeout=5000&_journal_mode=WAL`
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.Exec(`CREATE TABLE knowledge_bases (
		id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL, deleted_at DATETIME
	)`).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO knowledge_bases (id, tenant_id) VALUES (?, ?)", "kb-1", 7,
	).Error)

	sharedEntered := make(chan struct{})
	releaseShared := make(chan struct{})
	sharedDone := make(chan error, 1)
	go func() {
		sharedDone <- WithActiveShared(context.Background(), db, 7, "kb-1", func(_ *gorm.DB) error {
			close(sharedEntered)
			<-releaseShared
			return nil
		})
	}()
	select {
	case <-sharedEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("shared writer did not acquire its KB lock")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- db.Transaction(func(tx *gorm.DB) error {
			if _, err := LockExisting(tx, 7, "kb-1"); err != nil {
				return err
			}
			return tx.Exec(
				"UPDATE knowledge_bases SET deleted_at = ? WHERE id = ? AND tenant_id = ?",
				time.Now().UTC(), "kb-1", 7,
			).Error
		})
	}()
	select {
	case err := <-deleteDone:
		t.Fatalf("delete escaped the shared writer lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseShared)
	require.NoError(t, <-sharedDone)
	require.NoError(t, <-deleteDone)
	var callbackRan atomic.Bool
	err = WithActiveShared(context.Background(), db, 7, "kb-1", func(_ *gorm.DB) error {
		callbackRan.Store(true)
		return nil
	})
	require.ErrorIs(t, err, ErrKnowledgeBaseUnavailable)
	require.False(t, callbackRan.Load())
}

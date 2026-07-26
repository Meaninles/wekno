package iam

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestRunSyncUsesPostgresTransactionLockBeforeDeduplication(t *testing.T) {
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	db, err := gorm.Open(
		postgres.New(postgres.Config{Conn: sqlDB}),
		&gorm.Config{DisableAutomaticPing: true},
	)
	require.NoError(t, err)

	startedAt := time.Now().UTC()
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(\$1\)`).
		WithArgs(iamSyncAdvisoryLock).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(
		`SELECT \* FROM "custom_iam_sync_runs" WHERE status = \$1 .*LIMIT \$2`,
	).
		WithArgs("running", 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "triggered_by", "status", "started_at", "created_at", "updated_at",
		}).AddRow(
			"existing-run", "schedule", "running", startedAt, startedAt, startedAt,
		))
	mock.ExpectCommit()

	service := NewService(db, nil)
	run, err := service.RunSync(context.Background(), "schedule")
	require.NoError(t, err)
	require.NotNil(t, run)
	require.Equal(t, "existing-run", run.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRunSyncDeduplicatesAcrossPostgresServiceInstances(t *testing.T) {
	if os.Getenv("IAM_POSTGRES_INTEGRATION") != "1" {
		t.Skip("set IAM_POSTGRES_INTEGRATION=1 to run the PostgreSQL concurrency test")
	}

	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		envOrDefault("DB_HOST", "postgres"),
		envOrDefault("DB_PORT", "5432"),
		envOrDefault("DB_USER", "postgres"),
		os.Getenv("DB_PASSWORD"),
		envOrDefault("DB_NAME", "WeKnora"),
	)
	adminDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	schema := "iam_multi_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, adminDB.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	t.Cleanup(func() {
		_ = adminDB.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`).Error
	})

	openServiceDB := func() *gorm.DB {
		db, openErr := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		require.NoError(t, openErr)
		sqlDB, sqlErr := db.DB()
		require.NoError(t, sqlErr)
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		require.NoError(t, db.Exec(`SET search_path TO "`+schema+`"`).Error)
		t.Cleanup(func() { _ = sqlDB.Close() })
		return db
	}

	firstDB := openServiceDB()
	secondDB := openServiceDB()
	require.NoError(t, firstDB.AutoMigrate(&SyncSetting{}, &SyncRun{}))

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var startOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		startOnce.Do(func() { close(requestStarted) })
		<-releaseRequest
		http.Error(w, "intentional integration-test failure", http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	require.NoError(t, firstDB.Create(&SyncSetting{
		ID:               1,
		Enabled:          true,
		BaseURL:          server.URL,
		SyncClientID:     "integration-client",
		SyncClientSecret: "integration-secret",
		ScheduleMode:     ScheduleModeDaily,
		RunAt:            DefaultRunAt,
	}).Error)

	firstService := NewService(firstDB, nil)
	secondService := NewService(secondDB, nil)
	firstRun, err := firstService.RunSync(context.Background(), "schedule")
	require.NoError(t, err)
	require.NotNil(t, firstRun)

	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("first IAM sync did not reach the blocking upstream")
	}

	const callers = 24
	results := make(chan *SyncRun, callers)
	errors := make(chan error, callers)
	var callersWG sync.WaitGroup
	for i := 0; i < callers; i++ {
		callersWG.Add(1)
		go func(index int) {
			defer callersWG.Done()
			service := firstService
			if index%2 == 1 {
				service = secondService
			}
			run, runErr := service.RunSync(context.Background(), "schedule")
			results <- run
			errors <- runErr
		}(i)
	}
	callersWG.Wait()
	close(results)
	close(errors)

	for runErr := range errors {
		require.NoError(t, runErr)
	}
	for run := range results {
		require.NotNil(t, run)
		require.Equal(t, firstRun.ID, run.ID)
	}

	var runCount int64
	require.NoError(t, firstDB.Model(&SyncRun{}).Count(&runCount).Error)
	require.EqualValues(t, 1, runCount)

	close(releaseRequest)
	require.Eventually(t, func() bool {
		var run SyncRun
		if err := firstDB.First(&run, "id = ?", firstRun.ID).Error; err != nil {
			return false
		}
		return run.Status == "failed" && run.FinishedAt != nil
	}, 10*time.Second, 100*time.Millisecond)
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

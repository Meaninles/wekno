package processingtrace

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func traceRepositoryForTest(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	repository := NewRepository(db)
	require.NoError(t, repository.Migrate(context.Background()))
	return repository, db
}

func TestMigrateAddsAndBackfillsKnowledgeLifecycleColumnsOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
		CREATE TABLE knowledges (
			id TEXT PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL,
			parse_status TEXT NOT NULL,
			enable_status TEXT NOT NULL DEFAULT 'enabled',
			enrichment_status TEXT NOT NULL DEFAULT 'none',
			processed_at DATETIME,
			updated_at DATETIME,
			created_at DATETIME,
			deleted_at DATETIME
		)
	`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (
			id, tenant_id, knowledge_base_id, parse_status,
			enrichment_status, processed_at, updated_at, created_at
		) VALUES (
			'knowledge-1', 7, 'kb-1', 'finalizing',
			'completed', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)
	`).Error)

	repository := NewRepository(db)
	require.NoError(t, repository.Migrate(context.Background()))
	var row struct {
		CoreStatus            string
		CoreCompletedAt       *time.Time
		EnrichmentCompletedAt *time.Time
		EnrichmentError       string `gorm:"column:enrichment_error_summary"`
	}
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").Take(&row).Error)
	require.Equal(t, "ready", row.CoreStatus)
	require.NotNil(t, row.CoreCompletedAt)
	require.NotNil(t, row.EnrichmentCompletedAt)
	require.Empty(t, row.EnrichmentError)

	// Re-running migration must preserve an explicit post-install lifecycle
	// state rather than deriving it from parse_status again.
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").
		Update("core_status", "failed").Error)
	require.NoError(t, repository.Migrate(context.Background()))
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").Take(&row).Error)
	require.Equal(t, "failed", row.CoreStatus)
}

func TestMigratePromotesReceiptBackedFinalizingWithoutDroppingDerivativeState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
		CREATE TABLE knowledges (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, parse_status TEXT NOT NULL,
			processing_generation TEXT NOT NULL, processing_owner TEXT NOT NULL DEFAULT '',
			processing_fanout TEXT, pending_subtasks_count INTEGER NOT NULL DEFAULT 0,
			enable_status TEXT NOT NULL DEFAULT 'enabled',
			enrichment_status TEXT NOT NULL DEFAULT 'none',
			processed_at DATETIME, updated_at DATETIME, created_at DATETIME, deleted_at DATETIME
		)
	`).Error)
	require.NoError(t, db.Exec(`
		CREATE TABLE knowledge_fanout_completions (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			knowledge_base_id TEXT NOT NULL, processing_generation TEXT NOT NULL,
			item_id TEXT NOT NULL, completed_at DATETIME NOT NULL
		)
	`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (
			id, tenant_id, knowledge_base_id, parse_status, processing_generation,
			processing_owner, processing_fanout, pending_subtasks_count,
			enrichment_status, updated_at, created_at
		) VALUES ('knowledge-1', 7, 'kb-1', 'finalizing', 'generation-1',
			'old-owner', '{}', 5, 'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO knowledge_fanout_completions (
			tenant_id, knowledge_id, knowledge_base_id, processing_generation, item_id, completed_at
		) VALUES (7, 'knowledge-1', 'kb-1', 'generation-1', 'orchestration:postprocess', CURRENT_TIMESTAMP)
	`).Error)

	repository := NewRepository(db)
	require.NoError(t, repository.Migrate(context.Background()))
	var row struct {
		ParseStatus          string
		ProcessingOwner      string
		ProcessingFanout     *string
		PendingSubtasksCount int
		EnrichmentStatus     string
		ProcessedAt          *time.Time
	}
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").Take(&row).Error)
	require.Equal(t, "completed", row.ParseStatus)
	require.Empty(t, row.ProcessingOwner)
	require.Nil(t, row.ProcessingFanout)
	require.Equal(t, 5, row.PendingSubtasksCount)
	require.Equal(t, "pending", row.EnrichmentStatus)
	require.NotNil(t, row.ProcessedAt)
}

func TestLogicalSpanRetriesUpdateOneRow(t *testing.T) {
	repository, db := traceRepositoryForTest(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Second)
	progress := time.Now().UTC()
	require.NoError(t, repository.RecordBusinessProgress(ctx, Upsert{
		KnowledgeID: "knowledge-1", Attempt: 2, LogicalKey: "derivative:question_batch:0",
		Name: "question batch 0", Kind: "derivative", Status: "running",
		StartedAt: started, LastBusinessProgressAt: &progress,
	}))
	finished := time.Now().UTC()
	require.NoError(t, repository.RecordBusinessProgress(ctx, Upsert{
		KnowledgeID: "knowledge-1", Attempt: 2, LogicalKey: "derivative:question_batch:0",
		Name: "question batch 0", Kind: "derivative", Status: "failed",
		StartedAt: started, FinishedAt: &finished, IncrementRealAttempt: true,
		LastErrorCode: "provider_500", LastErrorMessage: "retryable",
	}))
	var rows []Span
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, 2, rows[0].RealAttemptCount)
	require.Equal(t, "failed", rows[0].Status)
	require.GreaterOrEqual(t, rows[0].DurationMS, int64(900))
}

func TestControlPlaneDeferralRollsBackSpeculativeAttempt(t *testing.T) {
	repository, db := traceRepositoryForTest(t)
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Second)
	progress := time.Now().UTC()
	require.NoError(t, repository.RecordBusinessProgress(ctx, Upsert{
		KnowledgeID: "knowledge-deferred", Attempt: 1, LogicalKey: "derivative:summary",
		Name: "summary", Kind: "derivative", Status: "running",
		StartedAt: started, LastBusinessProgressAt: &progress,
		IncrementRealAttempt: true,
	}))
	require.NoError(t, repository.RecordBusinessProgress(ctx, Upsert{
		KnowledgeID: "knowledge-deferred", Attempt: 1, LogicalKey: "derivative:summary",
		Name: "summary", Kind: "derivative", Status: "pending",
		StartedAt: started, LastBusinessProgressAt: &progress,
		DecrementRealAttempt: true,
	}))
	var row Span
	require.NoError(t, db.First(&row).Error)
	require.Equal(t, "pending", row.Status)
	require.Zero(t, row.RealAttemptCount)
	require.Nil(t, row.FinishedAt)
	require.Zero(t, row.DurationMS)
}

func TestListRequiresAttemptCapsAt500AndUsesCursor(t *testing.T) {
	repository, _ := traceRepositoryForTest(t)
	ctx := context.Background()
	for index := 0; index < 505; index++ {
		require.NoError(t, repository.RecordBusinessProgress(ctx, Upsert{
			KnowledgeID: "knowledge-1", Attempt: 1,
			LogicalKey: fmt.Sprintf("item:%04d", index),
			Name:       "item", Kind: "derivative", Status: "completed",
		}))
	}
	_, err := repository.List(ctx, "knowledge-1", 0, 100, nil)
	require.Error(t, err)
	first, err := repository.List(ctx, "knowledge-1", 1, 1000, nil)
	require.NoError(t, err)
	require.Len(t, first.Items, MaxPageSize)
	require.NotNil(t, first.NextCursor)
	second, err := repository.List(ctx, "knowledge-1", 1, 100, first.NextCursor)
	require.NoError(t, err)
	require.Len(t, second.Items, 5)
}

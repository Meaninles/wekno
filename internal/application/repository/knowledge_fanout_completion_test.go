package repository

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/custom/modules/questiondedup"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fanoutCompletionRepository interface {
	RecordKnowledgeFanoutCompletion(context.Context, uint64, string, string, string, string) (bool, error)
	ListKnowledgeFanoutCompletions(context.Context, uint64, string, string, string) ([]string, error)
	CountKnowledgeFanoutCompletions(context.Context, uint64, string, string, string) (int64, error)
	KnowledgeFanoutCompletionExists(context.Context, uint64, string, string, string, string) (bool, error)
	CleanupKnowledgeFanoutCompletions(context.Context, uint64, string, string, string) error
	FinalizeSubtaskGenerationItem(context.Context, uint64, string, string, string, string) (int, bool, error)
	FinalizeSubtaskGenerationItemOutcome(
		context.Context, uint64, string, string, string, string, string, string,
	) (int, bool, error)
	RecordGenerationOutcome(
		context.Context, uint64, string, string, string, string, string, string,
	) (bool, error)
	GetGenerationOutcomeAggregate(
		context.Context, uint64, string, string, string,
	) (enrichmentoutcome.Aggregate, error)
}

func newFanoutCompletionRepository(t *testing.T) (*gorm.DB, fanoutCompletionRepository) {
	t.Helper()
	dsn := fmt.Sprintf("file:fanout-completion-%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(
		&types.Knowledge{},
		&types.KnowledgeFanoutCompletion{},
		&enrichmentoutcome.Outcome{},
		&questiondedup.Claim{},
	); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS knowledge_tag_relations (knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS custom_processing_spans_v2 (knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS custom_document_split_plans (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS custom_document_split_parts (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS wiki_log_entries (tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS custom_content_cache_entries (
			tenant_id INTEGER NOT NULL, cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL, version_hash TEXT NOT NULL,
			ref_count INTEGER NOT NULL DEFAULT 0, updated_at DATETIME NOT NULL,
			PRIMARY KEY (tenant_id, cache_kind, content_hash, version_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS custom_content_cache_refs (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL, cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL, version_hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS embeddings (
			knowledge_id TEXT NOT NULL, knowledge_base_id TEXT,
			source_id TEXT,
			content TEXT NOT NULL DEFAULT '', deleted_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '', deleted_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS task_dead_letters (
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id INTEGER NOT NULL,
			task_type TEXT NOT NULL, scope TEXT NOT NULL, scope_id TEXT NOT NULL,
			related_id TEXT NOT NULL DEFAULT '', payload JSON NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE IF NOT EXISTS custom_derivative_work_items (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_derivative_provider_calls (
			id TEXT PRIMARY KEY, work_item_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_derivative_results (
			id TEXT PRIMARY KEY, work_item_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_document_queue_workflows (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_pending_ops (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tenant_id INTEGER NOT NULL, task_type TEXT NOT NULL,
			scope TEXT NOT NULL, scope_id TEXT NOT NULL,
			op TEXT NOT NULL, dedup_key TEXT NOT NULL DEFAULT '',
			payload JSON NOT NULL DEFAULT '{}',
			fail_count INTEGER NOT NULL DEFAULT 0,
			enqueued_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			claimed_at DATETIME
		)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	repo, ok := NewKnowledgeRepository(db).(fanoutCompletionRepository)
	if !ok {
		t.Fatal("knowledge repository does not expose durable fanout completion methods")
	}
	return db, repo
}

func TestGeneratedQuestionClaimsAreConcurrentGenerationScopedAndRetryStable(t *testing.T) {
	db, baseRepo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusFinalizing, 1)
	repo, ok := baseRepo.(interface {
		ClaimGeneratedQuestions(
			context.Context, uint64, string, string, string, []questiondedup.Candidate,
		) (map[string]string, bool, error)
	})
	require.True(t, ok)

	const contenders = 32
	var winners atomic.Int32
	errCh := make(chan error, contenders)
	var wg sync.WaitGroup
	for index := 0; index < contenders; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate, prepared := questiondedup.Prepare(
				fmt.Sprintf("question_batch[%d]:chunk:0", index),
				"采购审批必须在多长时间内完成？",
			)
			if !prepared {
				errCh <- fmt.Errorf("candidate %d was rejected", index)
				return
			}
			accepted, current, err := repo.ClaimGeneratedQuestions(
				context.Background(), 42, "knowledge-1", "kb-1", "generation-1",
				[]questiondedup.Candidate{candidate},
			)
			if err != nil {
				errCh <- err
				return
			}
			if !current {
				errCh <- fmt.Errorf("candidate %d saw a stale generation", index)
				return
			}
			if accepted[candidate.ClaimID] != "" {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	require.Equal(t, int32(1), winners.Load())

	var stored questiondedup.Claim
	require.NoError(t, db.Take(&stored).Error)
	retryCandidate, prepared := questiondedup.Prepare(
		stored.ClaimID,
		"重试时模型生成了不同措辞，但同一个输出槽必须复用首次提交的问题？",
	)
	require.True(t, prepared)
	accepted, current, err := repo.ClaimGeneratedQuestions(
		context.Background(), 42, "knowledge-1", "kb-1", "generation-1",
		[]questiondedup.Candidate{retryCandidate},
	)
	require.NoError(t, err)
	require.True(t, current)
	require.Equal(t, stored.Question, accepted[stored.ClaimID])
	var retryStableCount int64
	require.NoError(t, db.Model(&questiondedup.Claim{}).Count(&retryStableCount).Error)
	require.Equal(t, int64(1), retryStableCount)

	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id = ?", "knowledge-1").
		Updates(map[string]any{
			"processing_generation": "generation-2",
			"parse_status":          types.ParseStatusProcessing,
		}).Error)
	stale, current, err := repo.ClaimGeneratedQuestions(
		context.Background(), 42, "knowledge-1", "kb-1", "generation-1",
		[]questiondedup.Candidate{retryCandidate},
	)
	require.NoError(t, err)
	require.False(t, current)
	require.Empty(t, stale)
}

func TestGeneratedQuestionClaimsRejectSuperficialParaphrasesAcrossBatches(t *testing.T) {
	db, baseRepo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusFinalizing, 1)
	repo := baseRepo.(interface {
		ClaimGeneratedQuestions(
			context.Context, uint64, string, string, string, []questiondedup.Candidate,
		) (map[string]string, bool, error)
	})

	first, ok := questiondedup.Prepare("question_batch[0]:chunk-a:0", "采购审批必须在三个工作日内完成吗？")
	require.True(t, ok)
	second, ok := questiondedup.Prepare("question_batch[1]:chunk-b:0", "采购审批必须在五个工作日内完成吗？")
	require.True(t, ok)

	accepted, current, err := repo.ClaimGeneratedQuestions(
		context.Background(), 42, "knowledge-1", "kb-1", "generation-1",
		[]questiondedup.Candidate{first},
	)
	require.NoError(t, err)
	require.True(t, current)
	require.Equal(t, first.Question, accepted[first.ClaimID])

	accepted, current, err = repo.ClaimGeneratedQuestions(
		context.Background(), 42, "knowledge-1", "kb-1", "generation-1",
		[]questiondedup.Candidate{second},
	)
	require.NoError(t, err)
	require.True(t, current)
	require.Empty(t, accepted)

	var count int64
	require.NoError(t, db.Model(&questiondedup.Claim{}).Count(&count).Error)
	require.Equal(t, int64(1), count)
}

func TestGeneratedQuestionClaimsRemainCurrentAfterCoreCompletion(t *testing.T) {
	db, baseRepo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusCompleted, 1)
	repo := baseRepo.(interface {
		ClaimGeneratedQuestions(
			context.Context, uint64, string, string, string, []questiondedup.Candidate,
		) (map[string]string, bool, error)
	})
	candidate, ok := questiondedup.Prepare("question_batch[0]:chunk-a:0", "制度规定由哪个部门负责审批？")
	require.True(t, ok)
	accepted, current, err := repo.ClaimGeneratedQuestions(
		context.Background(), 42, "knowledge-1", "kb-1", "generation-1",
		[]questiondedup.Candidate{candidate},
	)
	require.NoError(t, err)
	require.True(t, current)
	require.Equal(t, candidate.Question, accepted[candidate.ClaimID])
}

func insertFanoutKnowledge(t *testing.T, db *gorm.DB, generation, status string, pending int) {
	t.Helper()
	knowledge := &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          status,
		ProcessingGeneration: generation,
		PendingSubtasksCount: pending,
		EnrichmentStatus: func() string {
			if status == types.ParseStatusFinalizing && pending > 0 {
				return types.EnrichmentStatusPending
			}
			return types.EnrichmentStatusNone
		}(),
	}
	if err := db.Create(knowledge).Error; err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeFanoutCompletionIsGenerationScopedAndIdempotent(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusProcessing, 0)
	ctx := context.Background()
	inserted, err := repo.RecordKnowledgeFanoutCompletion(ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0")
	if err != nil || !inserted {
		t.Fatalf("first insert = inserted:%v err:%v", inserted, err)
	}
	inserted, err = repo.RecordKnowledgeFanoutCompletion(ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0")
	if err != nil || inserted {
		t.Fatalf("duplicate insert = inserted:%v err:%v, want false/nil", inserted, err)
	}
	if err := db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("processing_generation", "generation-2").Error; err != nil {
		t.Fatal(err)
	}
	if inserted, err := repo.RecordKnowledgeFanoutCompletion(ctx, 42, "knowledge-1", "kb-1", "generation-2", "image:1"); err != nil || !inserted {
		t.Fatal(err)
	}
	items, err := repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0] != "image:0" {
		t.Fatalf("generation-1 items = %v, want [image:0]", items)
	}
	count, err := repo.CountKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-2")
	if err != nil || count != 1 {
		t.Fatalf("generation-2 count = %d err=%v, want 1/nil", count, err)
	}
	exists, err := repo.KnowledgeFanoutCompletionExists(ctx, 42, "knowledge-1", "kb-1", "generation-2", "image:1")
	if err != nil || !exists {
		t.Fatalf("generation-2 image exists = %v err=%v, want true/nil", exists, err)
	}
}

func TestPostProcessCompletionReceiptIsFinalizingSafeAndExcludedFromCoreCount(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusFinalizing, 1)
	ctx := context.Background()

	inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-1",
		processownership.PostProcessCompletionItem,
	)
	require.NoError(t, err)
	require.True(t, inserted)

	count, err := repo.CountKnowledgeFanoutCompletions(
		ctx, 42, "knowledge-1", "kb-1", "generation-1",
	)
	require.NoError(t, err)
	require.Zero(t, count, "orchestration receipts must not drain core fan-in")

	exists, err := repo.KnowledgeFanoutCompletionExists(
		ctx, 42, "knowledge-1", "kb-1", "generation-1",
		processownership.PostProcessCompletionItem,
	)
	require.NoError(t, err)
	require.True(t, exists)

	inserted, err = repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0",
	)
	require.NoError(t, err)
	require.False(t, inserted, "ordinary core items remain ineligible after finalizing")
}

func TestGenerationOutcomeIsImmutableFencedAndAggregated(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusProcessing, 0)
	ctx := context.Background()

	inserted, err := repo.RecordGenerationOutcome(
		ctx,
		42,
		"knowledge-1",
		"kb-1",
		"generation-1",
		"multimodal.image[0]",
		enrichmentoutcome.StatusFailed,
		"vlm unavailable",
	)
	require.NoError(t, err)
	require.True(t, inserted)

	// A duplicate delivery cannot rewrite the first terminal fact.
	inserted, err = repo.RecordGenerationOutcome(
		ctx,
		42,
		"knowledge-1",
		"kb-1",
		"generation-1",
		"multimodal.image[0]",
		enrichmentoutcome.StatusCompleted,
		"",
	)
	require.NoError(t, err)
	require.False(t, inserted)

	aggregate, err := repo.GetGenerationOutcomeAggregate(
		ctx, 42, "knowledge-1", "kb-1", "generation-1",
	)
	require.NoError(t, err)
	require.Equal(t, enrichmentoutcome.Aggregate{
		Total: 1, Failed: 1,
	}, aggregate)
	require.Equal(t, enrichmentoutcome.StatusFailed, aggregate.Status())

	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id = ?", "knowledge-1").
		Update("processing_generation", "generation-2").Error)
	inserted, err = repo.RecordGenerationOutcome(
		ctx,
		42,
		"knowledge-1",
		"kb-1",
		"generation-1",
		"multimodal.image[1]",
		enrichmentoutcome.StatusFailed,
		"stale worker",
	)
	require.NoError(t, err)
	require.False(t, inserted)
	aggregate, err = repo.GetGenerationOutcomeAggregate(
		ctx, 42, "knowledge-1", "kb-1", "generation-1",
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, aggregate.Total)
}

func TestKnowledgeFanoutCompletionRetentionAndStaleWorkerFence(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusProcessing, 0)
	ctx := context.Background()
	if inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0",
	); err != nil || !inserted {
		t.Fatalf("record old generation = %v/%v", inserted, err)
	}
	if err := db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("processing_generation", "generation-2").Error; err != nil {
		t.Fatal(err)
	}
	if inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-2", "image:1",
	); err != nil || !inserted {
		t.Fatalf("record current generation = %v/%v", inserted, err)
	}
	if err := repo.CleanupKnowledgeFanoutCompletions(
		ctx, 42, "knowledge-1", "kb-1", "generation-2",
	); err != nil {
		t.Fatal(err)
	}
	old, err := repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-1")
	if err != nil || len(old) != 0 {
		t.Fatalf("old generation after retention cleanup = %v err=%v", old, err)
	}
	current, err := repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-2")
	if err != nil || len(current) != 1 || current[0] != "image:1" {
		t.Fatalf("current generation after retention cleanup = %v err=%v", current, err)
	}

	// A delayed task from the deleted generation must not recreate the row.
	inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0",
	)
	if err != nil || inserted {
		t.Fatalf("stale delayed completion = inserted:%v err:%v, want false/nil", inserted, err)
	}
	old, err = repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-1")
	if err != nil || len(old) != 0 {
		t.Fatalf("stale generation was recreated = %v err=%v", old, err)
	}

	if err := repo.CleanupKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", ""); err != nil {
		t.Fatal(err)
	}
	current, err = repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-2")
	if err != nil || len(current) != 0 {
		t.Fatalf("all-generation cleanup = %v err=%v", current, err)
	}
}

func TestFinalizeSubtaskGenerationItemDrainsExactlyOnce(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusFinalizing, 2)
	ctx := context.Background()

	count, promoted, err := repo.FinalizeSubtaskGenerationItem(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "summary",
	)
	if err != nil || promoted || count != 1 {
		t.Fatalf("first summary drain = count:%d promoted:%v err:%v", count, promoted, err)
	}
	count, promoted, err = repo.FinalizeSubtaskGenerationItem(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "summary",
	)
	if err != nil || promoted || count != 1 {
		t.Fatalf("duplicate summary drain = count:%d promoted:%v err:%v", count, promoted, err)
	}
	count, promoted, err = repo.FinalizeSubtaskGenerationItem(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "question_batch[0]",
	)
	if err != nil || promoted || count != 0 {
		t.Fatalf("final distinct drain = count:%d promoted:%v err:%v", count, promoted, err)
	}
	var knowledge types.Knowledge
	if err := db.Where("id = ?", "knowledge-1").Take(&knowledge).Error; err != nil {
		t.Fatal(err)
	}
	if knowledge.ParseStatus != types.ParseStatusFinalizing || knowledge.PendingSubtasksCount != 0 {
		t.Fatalf("knowledge lifecycle = status:%s pending:%d", knowledge.ParseStatus, knowledge.PendingSubtasksCount)
	}
}

func TestFinalizeSubtaskOutcomeAggregatesConcurrentMixedResults(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	const descendants = 12
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusCompleted, descendants)
	ctx := context.Background()

	var promoted atomic.Int32
	errCh := make(chan error, descendants)
	var wg sync.WaitGroup
	for i := 0; i < descendants; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			status := enrichmentoutcome.StatusCompleted
			if i == 3 {
				status = enrichmentoutcome.StatusDegraded
			}
			if i == 8 {
				status = enrichmentoutcome.StatusFailed
			}
			_, won, err := repo.FinalizeSubtaskGenerationItemOutcome(
				ctx, 42, "knowledge-1", "kb-1", "generation-1",
				fmt.Sprintf("question_batch[%d]", i), status, "diagnostic",
			)
			if err != nil {
				errCh <- err
				return
			}
			if won {
				promoted.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := promoted.Load(); got != 0 {
		t.Fatalf("promotion winners = %d, want 0", got)
	}
	var knowledge types.Knowledge
	if err := db.Where("id = ?", "knowledge-1").Take(&knowledge).Error; err != nil {
		t.Fatal(err)
	}
	if knowledge.ParseStatus != types.ParseStatusCompleted ||
		knowledge.EnrichmentStatus != types.EnrichmentStatusDegraded {
		t.Fatalf(
			"lifecycle = parse:%s enrichment:%s, want completed/degraded",
			knowledge.ParseStatus, knowledge.EnrichmentStatus,
		)
	}
	var outcomeCount int64
	if err := db.Model(&enrichmentoutcome.Outcome{}).Count(&outcomeCount).Error; err != nil {
		t.Fatal(err)
	}
	if outcomeCount != descendants {
		t.Fatalf("outcome rows = %d, want %d", outcomeCount, descendants)
	}
}

func TestFinalizeSubtaskOutcomeAndWikiSettleAfterCoreCompletion(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusCompleted, 1)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").
		Update("wiki_status", types.WikiStatusPending).Error)

	count, promoted, err := repo.FinalizeSubtaskGenerationItemOutcome(
		context.Background(),
		42,
		"knowledge-1",
		"kb-1",
		"generation-1",
		"summary",
		enrichmentoutcome.StatusCompleted,
		"",
	)
	require.NoError(t, err)
	require.Zero(t, count)
	require.False(t, promoted)

	var waiting types.Knowledge
	require.NoError(t, db.Where("id = ?", "knowledge-1").Take(&waiting).Error)
	require.Equal(t, types.ParseStatusCompleted, waiting.ParseStatus)
	require.Equal(t, types.EnrichmentStatusCompleted, waiting.EnrichmentStatus)
	require.Equal(t, types.WikiStatusPending, waiting.WikiStatus)

	statusRepo, ok := repo.(interface {
		UpdateWikiStatusGeneration(
			context.Context, uint64, string, string, string, string, string,
		) (bool, error)
	})
	require.True(t, ok)
	updated, err := statusRepo.UpdateWikiStatusGeneration(
		context.Background(),
		42,
		"knowledge-1",
		"kb-1",
		"generation-1",
		types.WikiStatusCompleted,
		"",
	)
	require.NoError(t, err)
	require.True(t, updated)

	var completed types.Knowledge
	require.NoError(t, db.Where("id = ?", "knowledge-1").Take(&completed).Error)
	require.Equal(t, types.ParseStatusCompleted, completed.ParseStatus)
	require.Equal(t, types.EnrichmentStatusCompleted, completed.EnrichmentStatus)
	require.Equal(t, types.WikiStatusCompleted, completed.WikiStatus)
}

func TestFinalizeSubtaskOutcomeAllFailedIsExplicitlyFailed(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusFinalizing, 2)
	ctx := context.Background()
	for _, item := range []string{"summary", "graph_chunk[0]"} {
		if _, _, err := repo.FinalizeSubtaskGenerationItemOutcome(
			ctx, 42, "knowledge-1", "kb-1", "generation-1",
			item, enrichmentoutcome.StatusFailed, "upstream exhausted retries",
		); err != nil {
			t.Fatal(err)
		}
	}
	var knowledge types.Knowledge
	if err := db.Where("id = ?", "knowledge-1").Take(&knowledge).Error; err != nil {
		t.Fatal(err)
	}
	if knowledge.EnrichmentStatus != types.EnrichmentStatusFailed {
		t.Fatalf("enrichment status = %s, want failed", knowledge.EnrichmentStatus)
	}
}

func TestFinalizeSubtaskOutcomeFirstTerminalDeliveryIsImmutable(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusFinalizing, 2)
	ctx := context.Background()

	count, promoted, err := repo.FinalizeSubtaskGenerationItemOutcome(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "summary",
		enrichmentoutcome.StatusCompleted, "first terminal result",
	)
	require.NoError(t, err)
	require.False(t, promoted)
	require.Equal(t, 1, count)

	count, promoted, err = repo.FinalizeSubtaskGenerationItemOutcome(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "summary",
		enrichmentoutcome.StatusFailed, "late duplicate must not rewrite",
	)
	require.NoError(t, err)
	require.False(t, promoted)
	require.Equal(t, 1, count, "duplicate delivery must not consume another counter slot")

	var outcome enrichmentoutcome.Outcome
	require.NoError(t, db.Where(
		"tenant_id = ? AND knowledge_id = ? AND processing_generation = ? AND item_id = ?",
		42, "knowledge-1", "generation-1", "summary",
	).Take(&outcome).Error)
	require.Equal(t, enrichmentoutcome.StatusCompleted, outcome.Status)
	require.Equal(t, "first terminal result", outcome.Detail)

	_, promoted, err = repo.FinalizeSubtaskGenerationItemOutcome(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "graph_chunk[0]",
		enrichmentoutcome.StatusCompleted, "",
	)
	require.NoError(t, err)
	require.False(t, promoted)
	var knowledge types.Knowledge
	require.NoError(t, db.Where("id = ?", "knowledge-1").Take(&knowledge).Error)
	require.Equal(t, types.EnrichmentStatusCompleted, knowledge.EnrichmentStatus)
}

func TestDeleteKnowledgeSoftDeleteExplicitlyClearsFanoutLedger(t *testing.T) {
	db, repo := newFanoutCompletionRepository(t)
	insertFanoutKnowledge(t, db, "generation-1", types.ParseStatusProcessing, 0)
	ctx := context.Background()
	if inserted, err := repo.RecordKnowledgeFanoutCompletion(
		ctx, 42, "knowledge-1", "kb-1", "generation-1", "image:0",
	); err != nil || !inserted {
		t.Fatalf("record completion = %v/%v", inserted, err)
	}
	baseRepo := NewKnowledgeRepository(db)
	if err := baseRepo.DeleteKnowledge(ctx, 42, "knowledge-1"); err != nil {
		t.Fatal(err)
	}
	items, err := repo.ListKnowledgeFanoutCompletions(ctx, 42, "knowledge-1", "kb-1", "generation-1")
	if err != nil || len(items) != 0 {
		t.Fatalf("ledger after soft delete = %v err=%v", items, err)
	}
	var active int64
	if err := db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active knowledge rows = %d, want soft-deleted", active)
	}
}
